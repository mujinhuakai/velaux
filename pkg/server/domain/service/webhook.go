/*
Copyright 2021 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/emicklei/go-restful/v3"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"github.com/oam-dev/kubevela/pkg/policy/envbinding"

	workflowv1alpha1 "github.com/kubevela/workflow/api/v1alpha1"

	"github.com/kubevela/velaux/pkg/server/domain/model"
	"github.com/kubevela/velaux/pkg/server/infrastructure/datastore"
	assembler "github.com/kubevela/velaux/pkg/server/interfaces/api/assembler/v1"
	apisv1 "github.com/kubevela/velaux/pkg/server/interfaces/api/dto/v1"
	"github.com/kubevela/velaux/pkg/server/utils/bcode"
)

// WebhookService webhook service
type WebhookService interface {
	HandleApplicationWebhook(ctx context.Context, token string, req *restful.Request) (interface{}, error)
}

type webhookServiceImpl struct {
	Store              datastore.DataStore `inject:"datastore"`
	ApplicationService ApplicationService  `inject:""`
	WorkflowService    WorkflowService     `inject:""`
}

// WebhookHandlers is the webhook handlers
var WebhookHandlers []string

// TODO : add these vars to workflowv1alpha1
const (
	// ActionApprove approve a workflow step
	ActionApprove = "approve"
	// ActionRollback rollback a workflow step
	ActionRollback = "rollback"
	// ActionTerminate terminate a workflow step
	ActionTerminate = "terminate"
	// ActionExecute execute a workflow step
	ActionExecute = "execute"
)

// NewWebhookService new webhook service
func NewWebhookService() WebhookService {
	registerHandlers()
	return &webhookServiceImpl{}
}

func registerHandlers() {
	new(customHandlerImpl).install()
	new(acrHandlerImpl).install()
	new(dockerHubHandlerImpl).install()
	new(harborHandlerImpl).install()
	new(jfrogHandlerImpl).install()
}

type webhookHandler interface {
	handle(ctx context.Context, trigger *model.ApplicationTrigger, app *model.Application) (interface{}, error)
	install()
}

type customHandlerImpl struct {
	req apisv1.HandleApplicationTriggerWebhookRequest
	w   *webhookServiceImpl
}

type acrHandlerImpl struct {
	req apisv1.HandleApplicationTriggerACRRequest
	w   *webhookServiceImpl
}

type dockerHubHandlerImpl struct {
	req apisv1.HandleApplicationTriggerDockerHubRequest
	w   *webhookServiceImpl
}

func (c *webhookServiceImpl) newCustomHandler(req *restful.Request) (webhookHandler, error) {
	var webhookReq apisv1.HandleApplicationTriggerWebhookRequest
	if err := req.ReadEntity(&webhookReq); err != nil {
		return nil, bcode.ErrInvalidWebhookPayloadBody
	}
	return &customHandlerImpl{
		req: webhookReq,
		w:   c,
	}, nil
}

func (c *webhookServiceImpl) newACRHandler(req *restful.Request) (webhookHandler, error) {
	var acrReq apisv1.HandleApplicationTriggerACRRequest
	if err := req.ReadEntity(&acrReq); err != nil {
		return nil, bcode.ErrInvalidWebhookPayloadBody
	}
	return &acrHandlerImpl{
		req: acrReq,
		w:   c,
	}, nil
}

func (c *webhookServiceImpl) newDockerHubHandler(req *restful.Request) (webhookHandler, error) {
	var dockerHubReq apisv1.HandleApplicationTriggerDockerHubRequest
	if err := req.ReadEntity(&dockerHubReq); err != nil {
		return nil, bcode.ErrInvalidWebhookPayloadBody
	}
	return &dockerHubHandlerImpl{
		req: dockerHubReq,
		w:   c,
	}, nil
}

func (c *webhookServiceImpl) HandleApplicationWebhook(ctx context.Context, token string, req *restful.Request) (interface{}, error) {
	webhookTrigger := &model.ApplicationTrigger{
		Token: token,
	}
	if err := c.Store.Get(ctx, webhookTrigger); err != nil {
		if errors.Is(err, datastore.ErrRecordNotExist) {
			return nil, bcode.ErrInvalidWebhookToken
		}
		return nil, err
	}
	app := &model.Application{
		Name: webhookTrigger.AppPrimaryKey,
	}
	if err := c.Store.Get(ctx, app); err != nil {
		if errors.Is(err, datastore.ErrRecordNotExist) {
			return nil, bcode.ErrApplicationNotExist
		}
		return nil, err
	}

	var handler webhookHandler
	var err error
	switch webhookTrigger.PayloadType {
	case model.PayloadTypeCustom:
		handler, err = c.newCustomHandler(req)
		if err != nil {
			return nil, err
		}
	case model.PayloadTypeACR:
		handler, err = c.newACRHandler(req)
		if err != nil {
			return nil, err
		}
	case model.PayloadTypeHarbor:
		handler, err = c.newHarborHandler(req)
		if err != nil {
			return nil, err
		}
	case model.PayloadTypeDockerhub:
		handler, err = c.newDockerHubHandler(req)
		if err != nil {
			return nil, err
		}
	case model.PayloadTypeJFrog:
		handler, err = c.newJFrogHandler(req)
		if err != nil {
			return nil, err
		}
	default:
		return nil, bcode.ErrInvalidWebhookPayloadType
	}

	return handler.handle(ctx, webhookTrigger, app)
}

func (c *webhookServiceImpl) patchComponentProperties(ctx context.Context, component *model.ApplicationComponent, patch *runtime.RawExtension) error {
	merge, err := envbinding.MergeRawExtension(component.Properties.RawExtension(), patch)
	if err != nil {
		return err
	}
	prop, err := model.NewJSONStructByStruct(merge)
	if err != nil {
		return err
	}
	component.Properties = prop
	if err := c.Store.Put(ctx, component); err != nil {
		return err
	}
	return nil
}

func (c *customHandlerImpl) handle(ctx context.Context, webhookTrigger *model.ApplicationTrigger, app *model.Application) (interface{}, error) {
	workflow := &model.Workflow{
		AppPrimaryKey: webhookTrigger.AppPrimaryKey,
		Name:          webhookTrigger.WorkflowName,
	}
	if err := c.w.Store.Get(ctx, workflow); err != nil {
		return nil, err
	}

	switch c.req.Action {
	case ActionApprove:
		if err := c.w.WorkflowService.ResumeRecord(ctx, app, workflow, "", c.req.Step); err != nil {
			return nil, err
		}
		record := model.WorkflowRecord{
			AppPrimaryKey: workflow.AppPrimaryKey,
			WorkflowName:  workflow.Name,
		}
		records, err := c.w.Store.List(ctx, &record, &datastore.ListOptions{Page: 1, PageSize: 1, SortBy: []datastore.SortOption{
			{Key: "createTime", Order: datastore.SortOrderDescending},
		}})
		if err != nil {
			return nil, err
		}
		return &assembler.ConvertFromRecordModel(records[0].(*model.WorkflowRecord)).WorkflowRecordBase, nil
	case ActionTerminate:
		if err := c.w.WorkflowService.TerminateRecord(ctx, app, workflow, ""); err != nil {
			return nil, err
		}
		record := model.WorkflowRecord{
			AppPrimaryKey: workflow.AppPrimaryKey,
			WorkflowName:  workflow.Name,
		}
		records, err := c.w.Store.List(ctx, &record, &datastore.ListOptions{Page: 1, PageSize: 1, SortBy: []datastore.SortOption{
			{Key: "createTime", Order: datastore.SortOrderDescending},
		}})
		if err != nil {
			return nil, err
		}
		return &assembler.ConvertFromRecordModel(records[0].(*model.WorkflowRecord)).WorkflowRecordBase, nil
	case ActionRollback:
		workflowRecord := &model.WorkflowRecord{
			AppPrimaryKey: webhookTrigger.AppPrimaryKey,
			WorkflowName:  webhookTrigger.WorkflowName,
		}
		runningRecords, err := c.w.Store.List(ctx, workflowRecord, &datastore.ListOptions{
			Page:     1,
			PageSize: 1,
			SortBy:   []datastore.SortOption{{Key: "StartTime", Order: datastore.SortOrderDescending}},
			FilterOptions: datastore.FilterOptions{
				In: []datastore.InQueryOption{{Key: "status", Values: []string{string(workflowv1alpha1.WorkflowStepPhaseSuspending)}}},
			},
		})
		if len(runningRecords) == 0 {
			return nil, fmt.Errorf("no running records")
		}
		if err != nil {
			return nil, err
		}
		res, err := c.w.WorkflowService.RollbackRecord(ctx, app, workflow, runningRecords[0].PrimaryKey(), "")
		if err != nil {
			return nil, err
		}
		return res, nil
	case ActionExecute, "":
		{
			for comp, properties := range c.req.Upgrade {
				component := &model.ApplicationComponent{
					AppPrimaryKey: webhookTrigger.AppPrimaryKey,
					Name:          comp,
				}
				if err := c.w.Store.Get(ctx, component); err != nil {
					if errors.Is(err, datastore.ErrRecordNotExist) {
						return nil, bcode.ErrApplicationComponentNotExist
					}
					return nil, err
				}
				if err := c.w.patchComponentProperties(ctx, component, properties.RawExtension()); err != nil {
					return nil, err
				}
			}
			return c.w.ApplicationService.Deploy(ctx, app, apisv1.ApplicationDeployRequest{
				WorkflowName: webhookTrigger.WorkflowName,
				Note:         "triggered by webhook custom",
				TriggerType:  apisv1.TriggerTypeWebhook,
				Force:        true,
				CodeInfo:     c.req.CodeInfo,
			})
		}
	default:
		return nil, bcode.ErrInvalidWebhookPayloadBody
	}
}

func (c *customHandlerImpl) install() {
	WebhookHandlers = append(WebhookHandlers, model.PayloadTypeCustom)
}

func (c *acrHandlerImpl) handle(ctx context.Context, webhookTrigger *model.ApplicationTrigger, app *model.Application) (interface{}, error) {
	component, err := getComponent(ctx, c.w.Store, webhookTrigger)
	if err != nil {
		return nil, err
	}
	acrReq := c.req
	registry := webhookTrigger.Registry
	if registry == "" {
		registry = fmt.Sprintf("registry.%s.aliyuncs.com", acrReq.Repository.Region)
	}
	image := fmt.Sprintf("%s/%s:%s", registry, acrReq.Repository.RepoFullName, acrReq.PushData.Tag)
	if err := c.w.patchComponentProperties(ctx, component, &runtime.RawExtension{
		Raw: []byte(fmt.Sprintf(`{"image": "%s"}`, image)),
	}); err != nil {
		return nil, err
	}

	return c.w.ApplicationService.Deploy(ctx, app, apisv1.ApplicationDeployRequest{
		WorkflowName: webhookTrigger.WorkflowName,
		Note:         "triggered by webhook acr",
		TriggerType:  apisv1.TriggerTypeWebhook,
		Force:        true,
		ImageInfo: &model.ImageInfo{
			Type: model.PayloadTypeACR,
			Resource: &model.ImageResource{
				Digest:     acrReq.PushData.Digest,
				Tag:        acrReq.PushData.Tag,
				URL:        image,
				CreateTime: parseTimeString(acrReq.PushData.PushedAt),
			},
			Repository: &model.ImageRepository{
				Name:       acrReq.Repository.Name,
				Namespace:  acrReq.Repository.Namespace,
				FullName:   acrReq.Repository.RepoFullName,
				Region:     acrReq.Repository.Region,
				Type:       acrReq.Repository.RepoType,
				CreateTime: parseTimeString(acrReq.Repository.DateCreated),
			},
		},
	})
}

func (c *acrHandlerImpl) install() {
	WebhookHandlers = append(WebhookHandlers, model.PayloadTypeACR)
}

func (c dockerHubHandlerImpl) handle(ctx context.Context, trigger *model.ApplicationTrigger, app *model.Application) (interface{}, error) {
	dockerHubReq := c.req
	if dockerHubReq.Repository.Status != "Active" {
		klog.Infof("receive dockerhub webhook but not create event: %v", dockerHubReq)
		return &apisv1.ApplicationDockerhubWebhookResponse{
			State:       "failed",
			Description: "not create event",
		}, nil
	}
	component, err := getComponent(ctx, c.w.Store, trigger)
	if err != nil {
		return nil, err
	}
	image := fmt.Sprintf("docker.io/%s:%s", dockerHubReq.Repository.RepoName, dockerHubReq.PushData.Tag)
	if err := c.w.patchComponentProperties(ctx, component, &runtime.RawExtension{
		Raw: []byte(fmt.Sprintf(`{"image": "%s"}`, image)),
	}); err != nil {
		return nil, err
	}

	repositoryType := "public"
	if dockerHubReq.Repository.IsPrivate {
		repositoryType = "private"
	}

	if _, err = c.w.ApplicationService.Deploy(ctx, app, apisv1.ApplicationDeployRequest{
		WorkflowName: trigger.WorkflowName,
		Note:         "triggered by webhook dockerhub",
		TriggerType:  apisv1.TriggerTypeWebhook,
		Force:        true,
		ImageInfo: &model.ImageInfo{
			Type: model.PayloadTypeDockerhub,
			Resource: &model.ImageResource{
				Tag:        dockerHubReq.PushData.Tag,
				URL:        image,
				CreateTime: time.Unix(dockerHubReq.PushData.PushedAt, 0),
			},
			Repository: &model.ImageRepository{
				Name:       dockerHubReq.Repository.Name,
				Namespace:  dockerHubReq.Repository.Namespace,
				FullName:   dockerHubReq.Repository.RepoName,
				Type:       repositoryType,
				CreateTime: time.Unix(dockerHubReq.Repository.DateCreated, 0),
			},
		},
	}); err != nil {
		return nil, err
	}

	return &apisv1.ApplicationDockerhubWebhookResponse{
		State:       "success",
		Description: fmt.Sprintf("update application %s/%s success", app.Name, component.Name),
	}, nil
}

func (c dockerHubHandlerImpl) install() {
	WebhookHandlers = append(WebhookHandlers, model.PayloadTypeDockerhub)
}

func parseTimeString(t string) time.Time {
	if t == "" {
		return time.Time{}
	}

	l, err := time.LoadLocation("PRC")
	if err != nil {
		klog.Errorf("failed to load location: %v", err)
		return time.Time{}
	}
	parsedTime, err := time.ParseInLocation("2006-01-02 15:04:05", t, l)
	if err != nil {
		klog.Errorf("failed to parse time: %v", err)
		return time.Time{}
	}
	return parsedTime
}

type harborHandlerImpl struct {
	req apisv1.HandleApplicationHarborReq
	w   *webhookServiceImpl
}

func (c *webhookServiceImpl) newHarborHandler(req *restful.Request) (webhookHandler, error) {
	var harborReq apisv1.HandleApplicationHarborReq
	if err := req.ReadEntity(&harborReq); err != nil {
		return nil, bcode.ErrInvalidWebhookPayloadBody
	}
	if harborReq.Type != model.HarborEventTypePushArtifact {
		return nil, bcode.ErrInvalidWebhookPayloadBody
	}
	return &harborHandlerImpl{
		req: harborReq,
		w:   c,
	}, nil
}

func (c *harborHandlerImpl) install() {
	WebhookHandlers = append(WebhookHandlers, model.PayloadTypeHarbor)
}

func (c *harborHandlerImpl) handle(ctx context.Context, webhookTrigger *model.ApplicationTrigger, app *model.Application) (interface{}, error) {
	resources := c.req.EventData.Resources
	if len(resources) < 1 {
		return nil, bcode.ErrInvalidWebhookPayloadBody
	}
	imageURL := resources[0].ResourceURL
	digest := resources[0].Digest
	tag := resources[0].Tag
	component, err := getComponent(ctx, c.w.Store, webhookTrigger)
	if err != nil {
		return nil, err
	}
	harborReq := c.req
	if err := c.w.patchComponentProperties(ctx, component, &runtime.RawExtension{
		Raw: []byte(fmt.Sprintf(`{"image": "%s"}`, imageURL)),
	}); err != nil {
		return nil, err
	}
	return c.w.ApplicationService.Deploy(ctx, app, apisv1.ApplicationDeployRequest{
		WorkflowName: webhookTrigger.WorkflowName,
		Note:         "triggered by webhook harbor",
		TriggerType:  apisv1.TriggerTypeWebhook,
		Force:        true,
		ImageInfo: &model.ImageInfo{
			Type: model.PayloadTypeHarbor,
			Resource: &model.ImageResource{
				Digest:     digest,
				Tag:        tag,
				URL:        imageURL,
				CreateTime: time.Unix(harborReq.OccurAt, 0),
			},
			Repository: &model.ImageRepository{
				Name:       harborReq.EventData.Repository.Name,
				Namespace:  harborReq.EventData.Repository.Namespace,
				FullName:   harborReq.EventData.Repository.RepoFullName,
				Type:       harborReq.EventData.Repository.RepoType,
				CreateTime: time.Unix(harborReq.EventData.Repository.DateCreated, 0),
			},
		},
	})
}

type jfrogHandlerImpl struct {
	req apisv1.HandleApplicationTriggerJFrogRequest
	w   *webhookServiceImpl
}

func (c *webhookServiceImpl) newJFrogHandler(req *restful.Request) (webhookHandler, error) {
	var jfrogReq apisv1.HandleApplicationTriggerJFrogRequest
	if err := req.ReadEntity(&jfrogReq); err != nil {
		return nil, bcode.ErrInvalidWebhookPayloadBody
	}
	if jfrogReq.Domain != model.JFrogDomainDocker || jfrogReq.EventType != model.JFrogEventTypePush {
		return nil, bcode.ErrInvalidWebhookPayloadBody
	}
	// jfrog should use request header to give URL, it is not exist in request body
	jfrogReq.Data.URL = req.HeaderParameter("X-JFrogURL")
	return &jfrogHandlerImpl{
		req: jfrogReq,
		w:   c,
	}, nil
}

func (j *jfrogHandlerImpl) handle(ctx context.Context, webhookTrigger *model.ApplicationTrigger, app *model.Application) (interface{}, error) {
	jfrogReq := j.req
	component, err := getComponent(ctx, j.w.Store, webhookTrigger)
	if err != nil {
		return nil, err
	}
	image := fmt.Sprintf("%s/%s:%s", jfrogReq.Data.RepoKey, jfrogReq.Data.ImageName, jfrogReq.Data.Tag)
	pathArray := strings.Split(jfrogReq.Data.Path, "/")
	if len(pathArray) > 2 {
		image = fmt.Sprintf("%s/%s:%s", jfrogReq.Data.RepoKey, strings.Join(pathArray[:len(pathArray)-2], "/"), jfrogReq.Data.Tag)
	}

	if jfrogReq.Data.URL != "" {
		image = fmt.Sprintf("%s/%s", jfrogReq.Data.URL, image)
	}
	if err := j.w.patchComponentProperties(ctx, component, &runtime.RawExtension{
		Raw: []byte(fmt.Sprintf(`{"image": "%s"}`, image)),
	}); err != nil {
		return nil, err
	}

	return j.w.ApplicationService.Deploy(ctx, app, apisv1.ApplicationDeployRequest{
		WorkflowName: webhookTrigger.WorkflowName,
		Note:         "triggered by webhook jfrog",
		TriggerType:  apisv1.TriggerTypeWebhook,
		Force:        true,
		ImageInfo: &model.ImageInfo{
			Type: model.PayloadTypeJFrog,
			Resource: &model.ImageResource{
				Digest: jfrogReq.Data.Digest,
				Tag:    jfrogReq.Data.Tag,
				URL:    image,
			},
			Repository: &model.ImageRepository{
				Name:      jfrogReq.Data.ImageName,
				Namespace: jfrogReq.Data.RepoKey,
				FullName:  fmt.Sprintf("%s/%s", jfrogReq.Data.RepoKey, jfrogReq.Data.ImageName),
			},
		},
	})
}

func (j *jfrogHandlerImpl) install() {
	WebhookHandlers = append(WebhookHandlers, model.PayloadTypeJFrog)
}

func getComponent(ctx context.Context, ds datastore.DataStore, webhookTrigger *model.ApplicationTrigger) (*model.ApplicationComponent, error) {
	if webhookTrigger.ComponentName != "" {
		comp := &model.ApplicationComponent{
			AppPrimaryKey: webhookTrigger.AppPrimaryKey,
			Name:          webhookTrigger.ComponentName,
		}
		err := ds.Get(ctx, comp)
		if err != nil {
			return nil, err
		}
		return comp, nil
	}
	comp := &model.ApplicationComponent{
		AppPrimaryKey: webhookTrigger.AppPrimaryKey,
	}
	comps, err := ds.List(ctx, comp, &datastore.ListOptions{})
	if err != nil {
		return nil, err
	}
	if len(comps) == 0 {
		return nil, bcode.ErrApplicationComponentNotExist
	}
	return comps[0].(*model.ApplicationComponent), nil
}
