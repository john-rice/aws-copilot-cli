// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/copilot-cli/internal/pkg/aws/elbv2"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/copilot-cli/internal/pkg/aws/acm"
	awsecs "github.com/aws/copilot-cli/internal/pkg/aws/ecs"
	"github.com/aws/copilot-cli/internal/pkg/aws/partitions"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation/stack"
	"github.com/aws/copilot-cli/internal/pkg/deploy/upload/customresource"
	"github.com/aws/copilot-cli/internal/pkg/ecs"
	"github.com/aws/copilot-cli/internal/pkg/manifest"
	"github.com/aws/copilot-cli/internal/pkg/manifest/manifestinfo"
	"github.com/aws/copilot-cli/internal/pkg/template"
)

type backendSvcDeployer struct {
	*svcDeployer
	elbGetter  elbGetter
	backendMft *manifest.BackendService

	// Overriden in tests.
	aliasCertValidator aliasCertValidator
	newStack           func() cloudformation.StackConfiguration
}

// NewBackendDeployer is the constructor for backendSvcDeployer.
func NewBackendDeployer(in *WorkloadDeployerInput) (*backendSvcDeployer, error) {
	in.customResources = backendCustomResources
	svcDeployer, err := newSvcDeployer(in)
	if err != nil {
		return nil, err
	}
	bsMft, ok := in.Mft.(*manifest.BackendService)
	if !ok {
		return nil, fmt.Errorf("manifest is not of type %s", manifestinfo.BackendServiceType)
	}
	return &backendSvcDeployer{
		svcDeployer:        svcDeployer,
		elbGetter:          elbv2.New(svcDeployer.envSess),
		backendMft:         bsMft,
		aliasCertValidator: acm.New(svcDeployer.envSess),
	}, nil
}

func backendCustomResources(fs template.Reader) ([]*customresource.CustomResource, error) {
	crs, err := customresource.Backend(fs)
	if err != nil {
		return nil, fmt.Errorf("read custom resources for a %q: %w", manifestinfo.BackendServiceType, err)
	}
	return crs, nil
}

// IsServiceAvailableInRegion checks if service type exist in the given region.
func (backendSvcDeployer) IsServiceAvailableInRegion(region string) (bool, error) {
	return partitions.IsAvailableInRegion(awsecs.EndpointsID, region)
}

// UploadArtifacts uploads the deployment artifacts such as the container image, custom resources, addons and env files.
func (d *backendSvcDeployer) UploadArtifacts() (*UploadArtifactsOutput, error) {
	return d.uploadArtifacts(d.buildAndPushContainerImages, d.uploadArtifactsToS3, d.uploadCustomResources)
}

// GenerateCloudFormationTemplate generates a CloudFormation template and parameters for a workload.
func (d *backendSvcDeployer) GenerateCloudFormationTemplate(in *GenerateCloudFormationTemplateInput) (
	*GenerateCloudFormationTemplateOutput, error) {
	output, err := d.stackConfiguration(&in.StackRuntimeConfiguration)
	if err != nil {
		return nil, err
	}
	return d.generateCloudFormationTemplate(output.conf)
}

// DeployWorkload deploys a backend service using CloudFormation.
func (d *backendSvcDeployer) DeployWorkload(in *DeployWorkloadInput) (ActionRecommender, error) {
	stackConfigOutput, err := d.stackConfiguration(&in.StackRuntimeConfiguration)
	if err != nil {
		return nil, err
	}
	if err := d.deploy(in.Options, *stackConfigOutput); err != nil {
		return nil, err
	}
	return noopActionRecommender{}, nil
}

func (d *backendSvcDeployer) stackConfiguration(in *StackRuntimeConfiguration) (*svcStackConfigurationOutput, error) {
	rc, err := d.runtimeConfig(in)
	if err != nil {
		return nil, err
	}
	if err := d.validateALBRuntime(); err != nil {
		return nil, err
	}
	var opts []stack.BackendServiceOption
	if d.backendMft.HTTP.ImportedALB != nil {
		lb, err := d.elbGetter.LoadBalancer(aws.StringValue(d.backendMft.HTTP.ImportedALB))
		if err != nil {
			return nil, err
		}
		opts = append(opts, stack.WithImportedInternalALB(lb))
	}

	var conf cloudformation.StackConfiguration
	switch {
	case d.newStack != nil:
		conf = d.newStack()
	default:
		conf, err = stack.NewBackendService(stack.BackendServiceConfig{
			App:                d.app,
			EnvManifest:        d.envConfig,
			Manifest:           d.backendMft,
			RawManifest:        d.rawMft,
			ArtifactBucketName: d.resources.S3Bucket,
			ArtifactKey:        d.resources.KMSKeyARN,
			RuntimeConfig:      *rc,
			Addons:             d.addons,
		}, opts...)
		if err != nil {
			return nil, fmt.Errorf("create stack configuration: %w", err)
		}
	}

	return &svcStackConfigurationOutput{
		conf: cloudformation.WrapWithTemplateOverrider(conf, d.overrider),
		svcUpdater: d.newSvcUpdater(func(s *session.Session) serviceForceUpdater {
			return ecs.New(s)
		}),
	}, nil
}

func (d *backendSvcDeployer) validateALBRuntime() error {
	if d.backendMft.HTTP.IsEmpty() {
		return nil
	}
	if err := d.validateImportedALBConfig(); err != nil {
		return fmt.Errorf(`validate imported ALB configuration for "http": %w`, err)
	}
	if err := d.validateRuntimeRoutingRule(d.backendMft.HTTP.Main); err != nil {
		return fmt.Errorf(`validate ALB runtime configuration for "http": %w`, err)
	}
	for idx, rule := range d.backendMft.HTTP.AdditionalRoutingRules {
		if err := d.validateRuntimeRoutingRule(rule); err != nil {
			return fmt.Errorf(`validate ALB runtime configuration for "http.additional_rules[%d]": %w`, idx, err)
		}
	}
	return nil
}

func (d *backendSvcDeployer) validateImportedALBConfig() error {
	if d.backendMft.HTTP.ImportedALB == nil {
		return nil
	}
	alb, err := d.elbGetter.LoadBalancer(aws.StringValue(d.backendMft.HTTP.ImportedALB))
	if err != nil {
		return fmt.Errorf(`retrieve load balancer %q: %w`, aws.StringValue(d.backendMft.HTTP.ImportedALB), err)
	}
	if alb.Scheme != "internal" {
		return fmt.Errorf(`imported ALB %q for Backend Service %q should have "internal" Scheme value`, alb.ARN, aws.StringValue(d.backendMft.Name))
	}
	if len(alb.Listeners) == 0 {
		return fmt.Errorf(`imported ALB %q must have at least one listener. For two listeners, one must be of protocol HTTP and the other of protocol HTTPS`, alb.ARN)
	}
	if len(alb.Listeners) == 1 {
		return nil
	}
	var quantHTTP, quantHTTPS int
	for _, listener := range alb.Listeners {
		if listener.Protocol == "HTTP" {
			quantHTTP += 1
		} else if listener.Protocol == "HTTPS" {
			quantHTTPS += 1
		}
	}
	if quantHTTP != 1 || quantHTTPS != 1 {
		return fmt.Errorf("imported ALB %q must have exactly one listener of protocol HTTP and exactly one listener of protocol HTTPS", alb.ARN)
	}
	return nil
}

func (d *backendSvcDeployer) validateRuntimeRoutingRule(rule manifest.RoutingRule) error {
	if rule.IsEmpty() {
		return nil
	}
	hasImportedCerts := len(d.envConfig.HTTPConfig.Private.Certificates) != 0
	switch {
	case rule.Alias.IsEmpty() && hasImportedCerts:
		return &errSvcWithNoALBAliasDeployingToEnvWithImportedCerts{
			name:    d.name,
			envName: d.env.Name,
		}
	case rule.Alias.IsEmpty():
		return nil
	case !hasImportedCerts:
		return fmt.Errorf(`cannot specify "alias" in an environment without imported certs`)
	}

	aliases, err := rule.Alias.ToStringSlice()
	if err != nil {
		return fmt.Errorf("convert aliases to string slice: %w", err)
	}

	if err := d.aliasCertValidator.ValidateCertAliases(aliases, d.envConfig.HTTPConfig.Private.Certificates); err != nil {
		return fmt.Errorf("validate aliases against the imported certificate for env %s: %w", d.env.Name, err)
	}
	return nil
}
