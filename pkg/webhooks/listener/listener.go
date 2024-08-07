package listener

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-logr/logr"
	jd "github.com/josephburnett/jd/lib"
	"gopkg.in/yaml.v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/reborn1867/k8s-resource-tracer/pkg/git"
)

type ListenerWebhook struct {
	Logger          logr.Logger
	EnableGitReview bool
	GitConfig
}

type GitConfig struct {
	GitPath   string
	SubPath   string
	GitBranch string
	GitAuth   transport.AuthMethod
}

type CustomRenderOption struct {
	jd.Metadata
}

func (c *CustomRenderOption) is_render_option() {}

func (l *ListenerWebhook) Handle(ctx context.Context, r admission.Request) admission.Response {
	obj := map[string]interface{}{}
	if err := json.Unmarshal(r.Object.Raw, &obj); err != nil {
		l.Logger.Error(err, "failed to unmarshal raw object")
		return admission.Errored(400, err)
	}

	oldObj := map[string]interface{}{}
	if err := json.Unmarshal(r.OldObject.Raw, &oldObj); err != nil {
		l.Logger.Error(err, "failed to unmarshal old raw object")
		return admission.Errored(400, err)
	}

	oldRaw, err := jd.NewJsonNode(oldObj)
	if err != nil {
		l.Logger.Error(err, "failed to read old object")
		return admission.Errored(400, err)
	}

	raw, err := jd.NewJsonNode(obj)
	if err != nil {
		l.Logger.Error(err, "failed to read current object")
		return admission.Errored(400, err)
	}

	currentSpec, err := jd.NewJsonNode(obj["spec"])
	if err != nil {
		l.Logger.Error(err, "failed to read spec of current object")
		return admission.Errored(400, err)
	}
	oldSpec, err := jd.NewJsonNode(oldObj["spec"])
	if err != nil {
		l.Logger.Error(err, "failed to read spec of old object")
		return admission.Errored(400, err)
	}

	currentStatus, err := jd.NewJsonNode(obj["status"])
	if err != nil {
		l.Logger.Error(err, "failed to read status of current object")
		return admission.Errored(400, err)
	}
	oldStatus, err := jd.NewJsonNode(oldObj["status"])
	if err != nil {
		l.Logger.Error(err, "failed to read status of old object")
		return admission.Errored(400, err)
	}

	newMetaData := obj["metadata"].(map[string]interface{})
	oldMetadata := oldObj["metadata"].(map[string]interface{})
	newLabels, err := jd.NewJsonNode(newMetaData["labels"])
	if err != nil {
		l.Logger.Error(err, "failed to read labels of current object")
		return admission.Errored(400, err)
	}

	oldLabels, err := jd.NewJsonNode(oldMetadata["labels"])
	if err != nil {
		l.Logger.Error(err, "failed to read labels of old object")
		return admission.Errored(400, err)
	}

	newAnnotations, err := jd.NewJsonNode(newMetaData["annotations"])
	if err != nil {
		l.Logger.Error(err, "failed to read annotations of current object")
		return admission.Errored(400, err)
	}
	oldAnnotations, err := jd.NewJsonNode(oldMetadata["annotations"])
	if err != nil {
		l.Logger.Error(err, "failed to read annotations of old object")
		return admission.Errored(400, err)
	}

	var fieldManagers []string
	for _, f := range newMetaData["managedFields"].([]interface{}) {
		fieldManagers = append(fieldManagers, f.(map[string]interface{})["manager"].(string))
	}

	latestManager := fieldManagers[len(fieldManagers)-1]

	l.Logger.Info("Captured request", "userInfo", r.UserInfo, "operation", r.Operation, "resource", r.Resource.String(), "name", r.Name, "namespace", r.Namespace, "last updated manager", latestManager)

	specDiff := oldSpec.Diff(currentSpec).Render(jd.COLOR)
	statusDiff := oldStatus.Diff(currentStatus).Render(jd.COLOR)
	labelsDiff := oldLabels.Diff(newLabels).Render(jd.COLOR)
	annotationsDiff := oldAnnotations.Diff(newAnnotations).Render(jd.COLOR)

	if specDiff == "" && statusDiff == "" && labelsDiff == "" && annotationsDiff == "" {
		l.Logger.Info("No changes detected")
	} else {
		fmt.Printf("spec diff: \n%s\n", specDiff)
		fmt.Printf("status diff: \n%s\n", statusDiff)
		fmt.Printf("labels diff: \n%s\n", labelsDiff)
		fmt.Printf("annotation diff: \n%s\n", annotationsDiff)

		if l.Logger.V(1).Enabled() {
			l.Logger.V(1).Info("raw diff of the whole objects")
			fmt.Printf("raw diff: \n%s\n", oldRaw.Diff(raw).Render(jd.COLOR))
		}

		if l.EnableGitReview {
			gvk := buildGVK(obj)
			fileName := fmt.Sprintf("%s.yaml", newMetaData["name"].(string))
			subpath := filepath.Join(l.SubPath, newMetaData["namespace"].(string), gvk, fileName)

			delete(obj["metadata"].(map[string]interface{}), "managedFields")
			yamlOutput, err := yaml.Marshal(obj)
			if err != nil {
				l.Logger.Error(err, "failed to covert to yaml output")
			}

			if err := l.syncGit(subpath, r.UserInfo.Username, latestManager, yamlOutput); err != nil {
				l.Logger.Error(err, "failed to sync git")
			}
		}
	}

	return admission.Allowed("allowed")
}

func (l *ListenerWebhook) syncGit(subpath, userInfo, fieldManager string, data []byte) error {
	if err := git.CommitChange(l.GitPath, subpath, userInfo, fieldManager, data, l.Logger); err != nil {
		return fmt.Errorf("failed to commit new object: %s", err)
	}
	l.Logger.Info("git commit successfully", "author", userInfo)

	if err := git.PushToRemote(l.GitPath, l.GitAuth); err != nil {
		return fmt.Errorf("failed to push to remote: %s", err)
	}

	l.Logger.Info("git push to remote successfully")

	return nil
}

func buildGVK(obj map[string]interface{}) string {
	apiVersion := obj["apiVersion"].(string)
	gv := strings.ReplaceAll(apiVersion, "/", "-")
	return fmt.Sprintf("%s.%s", gv, obj["kind"].(string))
}
