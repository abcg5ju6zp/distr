package main

import (
	"context"
	"errors"
	"strings"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/distr-sh/distr/internal/controllerapply"
	"github.com/docker/cli/cli/compose/convert"
	composeapi "github.com/docker/compose/v5/pkg/api"
	mobyswarm "github.com/moby/moby/api/types/swarm"
	mobyClient "github.com/moby/moby/client"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

type mobyService = mobyswarm.Service

// inspectComposeResources lists the containers currently labelled with the compose project.
func inspectComposeResources(ctx context.Context, projectName string) []composeapi.ContainerSummary {
	summaries, err := composeService.Ps(ctx, projectName, composeapi.PsOptions{All: true})
	if err != nil {
		logger.Debug("could not inspect compose resources", zap.String("project", projectName), zap.Error(err))
		return nil
	}
	return summaries
}

func composeResourceIdentities(summaries []composeapi.ContainerSummary) []controllerapply.ResourceIdentity {
	resources := make([]controllerapply.ResourceIdentity, 0, len(summaries))
	for _, summary := range summaries {
		resources = append(resources, controllerapply.ResourceIdentity{
			Kind:     "container",
			ID:       summary.ID,
			Name:     summary.Name,
			Image:    summary.Image,
			State:    string(summary.State),
			Networks: summary.Networks,
			Labels:   summary.Labels,
		})
	}
	return resources
}

// composeResourcesMatch reports whether every service declared in the project already has a container
// running its declared image. A project fully present is adopted instead of applied again, a project
// present with old images is converged and a missing project is applied for the first time.
func composeResourcesMatch(summaries []composeapi.ContainerSummary, project *composetypes.Project) bool {
	if len(project.Services) == 0 || len(summaries) == 0 {
		return false
	}
	have := make(map[string]string, len(summaries))
	for _, summary := range summaries {
		have[summary.Service] = controllerapply.CanonicalImageRef(summary.Image)
	}
	for name, service := range project.Services {
		image, ok := have[name]
		if !ok || image == "" {
			return false
		}
		if expected := controllerapply.CanonicalImageRef(service.Image); expected != "" && image != expected {
			return false
		}
	}
	return true
}

// inspectSwarmResources lists the services currently labelled with the stack name.
func inspectSwarmResources(ctx context.Context, projectName string) ([]mobyService, error) {
	result, err := dockerCli.Client().ServiceList(ctx, mobyClient.ServiceListOptions{
		Filters: mobyClient.Filters{}.Add("label", convert.LabelNamespace+"="+projectName),
		Status:  true,
	})
	if err != nil {
		return nil, err
	}
	return result.Items, nil
}

func swarmResourceIdentities(services []mobyService) []controllerapply.ResourceIdentity {
	resources := make([]controllerapply.ResourceIdentity, 0, len(services))
	for _, service := range services {
		image := ""
		if service.Spec.TaskTemplate.ContainerSpec != nil {
			image = service.Spec.TaskTemplate.ContainerSpec.Image
		}
		resources = append(resources, controllerapply.ResourceIdentity{
			Kind:   "service",
			ID:     service.ID,
			Name:   service.Spec.Name,
			Image:  image,
			Labels: service.Spec.Labels,
		})
	}
	return resources
}

// swarmResourcesMatch compares deployed services with the compose specification the same way
// "docker stack deploy" names them, "<stack>_<service>", and compares images canonically.
func swarmResourcesMatch(services []mobyService, projectName string, expected map[string]string) bool {
	if len(expected) == 0 || len(services) == 0 {
		return false
	}
	have := make(map[string]string, len(services))
	for _, service := range services {
		if service.Spec.TaskTemplate.ContainerSpec != nil {
			have[service.Spec.Name] = controllerapply.CanonicalImageRef(
				service.Spec.TaskTemplate.ContainerSpec.Image)
		}
	}
	for name, image := range expected {
		actual, ok := have[projectName+"_"+name]
		if !ok || actual == "" {
			return false
		}
		if expectedImage := controllerapply.CanonicalImageRef(image); expectedImage != "" && actual != expectedImage {
			return false
		}
	}
	return true
}

// specServiceImages returns the images declared in a compose file.
func specServiceImages(composeFile []byte) []string {
	expected := swarmExpectedServices(composeFile)
	images := make([]string, 0, len(expected))
	for _, image := range expected {
		if image != "" {
			images = append(images, image)
		}
	}
	return images
}

// swarmExpectedServices maps service names to images as declared in the name-stripped swarm compose file.
func swarmExpectedServices(composeFile []byte) map[string]string {
	var parsed struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeFile, &parsed); err != nil {
		return nil
	}
	services := make(map[string]string, len(parsed.Services))
	for name, service := range parsed.Services {
		services[name] = service.Image
	}
	return services
}

// isAmbiguousEngineError reports whether an engine error may still have changed resources. Such errors
// have to be reconciled against actual resources instead of being retried blindly or marked failed.
func isAmbiguousEngineError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"error during connect",
		"connection refused",
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"timeout while awaiting",
		"client.timeout exceeded",
		"no such host",
		"server misbehaving",
		"temporarily unavailable",
		"signal: killed",
		"eof",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

// errOutcomeUnknown marks an engine call whose effect could not be determined. The generation stays
// claimed and is reconciled from its receipt and the actual resources on the next tick.
var errOutcomeUnknown = errors.New("container engine action result is unknown")

func receiptNow() time.Time { return time.Now().UTC() }
