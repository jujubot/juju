// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

// Package cloud defines an API end point for functions dealing with
// the controller's cloud definition, and cloud credentials.
package cloud

import (
	"fmt"

	"github.com/juju/errors"
	"gopkg.in/juju/names.v2"

	"github.com/juju/juju/apiserver/common"
	"github.com/juju/juju/apiserver/facade"
	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/cloud"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/permission"
	"github.com/juju/juju/state"
)

type CloudV1 interface {
	Clouds() (params.CloudsResult, error)
	Cloud(args params.Entities) (params.CloudResults, error)
	DefaultCloud() (params.StringResult, error)
	UserCredentials(args params.UserClouds) (params.StringsResults, error)
	UpdateCredentials(args params.TaggedCredentials) (params.ErrorResults, error)
	RevokeCredentials(args params.Entities) (params.ErrorResults, error)
	Credential(args params.Entities) (params.CloudCredentialResults, error)
}

type CloudV2 interface {
	AddCloud(cloudArgs params.AddCloudArgs) error
	AddCredentials(args params.TaggedCredentials) (params.ErrorResults, error)
	CredentialContents(credentialArgs params.CloudCredentialArgs) (params.CredentialContentResults, error)
}

type CloudAPI struct {
	backend                Backend
	ctlrBackend            Backend
	authorizer             facade.Authorizer
	apiUser                names.UserTag
	getCredentialsAuthFunc common.GetAuthFunc
}

type CloudAPIV2 struct {
	CloudAPI
}

var (
	_ CloudV1 = (*CloudAPI)(nil)
	_ CloudV2 = (*CloudAPIV2)(nil)
)

// NewFacade provides the required signature for facade registration.
func NewFacade(context facade.Context) (*CloudAPI, error) {
	st := NewStateBackend(context.State())
	ctlrSt := NewStateBackend(context.StatePool().SystemState())
	return NewCloudAPI(st, ctlrSt, context.Auth())
}

func NewFacadeV2(context facade.Context) (*CloudAPIV2, error) {
	st := NewStateBackend(context.State())
	ctlrSt := NewStateBackend(context.StatePool().SystemState())
	return NewCloudAPIV2(st, ctlrSt, context.Auth())
}

// NewCloudAPI creates a new API server endpoint for managing the controller's
// cloud definition and cloud credentials.
func NewCloudAPI(backend, ctlrBackend Backend, authorizer facade.Authorizer) (*CloudAPI, error) {
	if !authorizer.AuthClient() {
		return nil, common.ErrPerm
	}

	authUser, _ := authorizer.GetAuthTag().(names.UserTag)
	getUserAuthFunc := func() (common.AuthFunc, error) {
		isAdmin, err := authorizer.HasPermission(permission.SuperuserAccess, backend.ControllerTag())
		if err != nil && !errors.IsNotFound(err) {
			return nil, err
		}
		return func(tag names.Tag) bool {
			userTag, ok := tag.(names.UserTag)
			if !ok {
				return false
			}
			return isAdmin || userTag == authUser
		}, nil
	}
	return &CloudAPI{
		backend:                backend,
		ctlrBackend:            ctlrBackend,
		authorizer:             authorizer,
		getCredentialsAuthFunc: getUserAuthFunc,
		apiUser:                authUser,
	}, nil
}

func NewCloudAPIV2(backend, ctlrBackend Backend, authorizer facade.Authorizer) (*CloudAPIV2, error) {
	cloudAPI, err := NewCloudAPI(backend, ctlrBackend, authorizer)
	if err != nil {
		return nil, err
	}
	return &CloudAPIV2{
		CloudAPI: *cloudAPI,
	}, nil
}

// Clouds returns the definitions of all clouds supported by the controller.
func (api *CloudAPI) Clouds() (params.CloudsResult, error) {
	var result params.CloudsResult
	clouds, err := api.backend.Clouds()
	if err != nil {
		return result, err
	}
	result.Clouds = make(map[string]params.Cloud)
	for tag, cloud := range clouds {
		paramsCloud := common.CloudToParams(cloud)
		result.Clouds[tag.String()] = paramsCloud
	}
	return result, nil
}

// Cloud returns the cloud definitions for the specified clouds.
func (api *CloudAPI) Cloud(args params.Entities) (params.CloudResults, error) {
	results := params.CloudResults{
		Results: make([]params.CloudResult, len(args.Entities)),
	}
	one := func(arg params.Entity) (*params.Cloud, error) {
		tag, err := names.ParseCloudTag(arg.Tag)
		if err != nil {
			return nil, err
		}
		cloud, err := api.backend.Cloud(tag.Id())
		if err != nil {
			return nil, err
		}
		paramsCloud := common.CloudToParams(cloud)
		return &paramsCloud, nil
	}
	for i, arg := range args.Entities {
		cloud, err := one(arg)
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
		} else {
			results.Results[i].Cloud = cloud
		}
	}
	return results, nil
}

// DefaultCloud returns the tag of the cloud that models will be
// created in by default.
func (api *CloudAPI) DefaultCloud() (params.StringResult, error) {
	controllerModel, err := api.ctlrBackend.Model()
	if err != nil {
		return params.StringResult{}, err
	}
	return params.StringResult{
		Result: names.NewCloudTag(controllerModel.Cloud()).String(),
	}, nil
}

// UserCredentials returns the cloud credentials for a set of users.
func (api *CloudAPI) UserCredentials(args params.UserClouds) (params.StringsResults, error) {
	results := params.StringsResults{
		Results: make([]params.StringsResult, len(args.UserClouds)),
	}
	authFunc, err := api.getCredentialsAuthFunc()
	if err != nil {
		return results, err
	}
	for i, arg := range args.UserClouds {
		userTag, err := names.ParseUserTag(arg.UserTag)
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}
		if !authFunc(userTag) {
			results.Results[i].Error = common.ServerError(common.ErrPerm)
			continue
		}
		cloudTag, err := names.ParseCloudTag(arg.CloudTag)
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}
		cloudCredentials, err := api.backend.CloudCredentials(userTag, cloudTag.Id())
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}
		out := make([]string, 0, len(cloudCredentials))
		for tagId := range cloudCredentials {
			out = append(out, names.NewCloudCredentialTag(tagId).String())
		}
		results.Results[i].Result = out
	}
	return results, nil
}

// AddCredentials adds new credentials.
// In contrast to UpdateCredentials() below, the new credentials can be
// for a cloud that the controller does not manage (this is required
// for CAAS models)
func (api *CloudAPIV2) AddCredentials(args params.TaggedCredentials) (params.ErrorResults, error) {
	results := params.ErrorResults{
		Results: make([]params.ErrorResult, len(args.Credentials)),
	}

	authFunc, err := api.getCredentialsAuthFunc()
	if err != nil {
		return results, err
	}
	for i, arg := range args.Credentials {
		tag, err := names.ParseCloudCredentialTag(arg.Tag)
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}
		// NOTE(axw) if we add ACLs for cloud credentials, we'll need
		// to change this auth check.
		if !authFunc(tag.Owner()) {
			results.Results[i].Error = common.ServerError(common.ErrPerm)
			continue
		}

		in := cloud.NewCredential(
			cloud.AuthType(arg.Credential.AuthType),
			arg.Credential.Attributes,
		)
		if err := api.backend.UpdateCloudCredential(tag, in); err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}
	}
	return results, nil
}

// UpdateCredentials updates a set of cloud credentials.
func (api *CloudAPI) UpdateCredentials(args params.TaggedCredentials) (params.ErrorResults, error) {
	results := params.ErrorResults{
		Results: make([]params.ErrorResult, len(args.Credentials)),
	}
	authFunc, err := api.getCredentialsAuthFunc()
	if err != nil {
		return results, err
	}
	for i, arg := range args.Credentials {
		tag, err := names.ParseCloudCredentialTag(arg.Tag)
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}
		// NOTE(axw) if we add ACLs for cloud credentials, we'll need
		// to change this auth check.
		if !authFunc(tag.Owner()) {
			results.Results[i].Error = common.ServerError(common.ErrPerm)
			continue
		}
		in := cloud.NewCredential(
			cloud.AuthType(arg.Credential.AuthType),
			arg.Credential.Attributes,
		)
		if err := api.backend.UpdateCloudCredential(tag, in); err != nil {
			if errors.IsNotFound(err) {
				err = errors.Errorf(
					"cannot update credential %q: controller does not manage cloud %q",
					tag.Name(), tag.Cloud().Id())
			}
			results.Results[i].Error = common.ServerError(err)
			continue
		}
	}
	return results, nil
}

// RevokeCredentials revokes a set of cloud credentials.
func (api *CloudAPI) RevokeCredentials(args params.Entities) (params.ErrorResults, error) {
	results := params.ErrorResults{
		Results: make([]params.ErrorResult, len(args.Entities)),
	}
	authFunc, err := api.getCredentialsAuthFunc()
	if err != nil {
		return results, err
	}
	for i, arg := range args.Entities {
		tag, err := names.ParseCloudCredentialTag(arg.Tag)
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}
		// NOTE(axw) if we add ACLs for cloud credentials, we'll need
		// to change this auth check.
		if !authFunc(tag.Owner()) {
			results.Results[i].Error = common.ServerError(common.ErrPerm)
			continue
		}
		if err := api.backend.RemoveCloudCredential(tag); err != nil {
			results.Results[i].Error = common.ServerError(err)
		}
	}
	return results, nil
}

// Credential returns the specified cloud credential for each tag, minus secrets.
func (api *CloudAPI) Credential(args params.Entities) (params.CloudCredentialResults, error) {
	results := params.CloudCredentialResults{
		Results: make([]params.CloudCredentialResult, len(args.Entities)),
	}
	authFunc, err := api.getCredentialsAuthFunc()
	if err != nil {
		return results, err
	}

	for i, arg := range args.Entities {
		credentialTag, err := names.ParseCloudCredentialTag(arg.Tag)
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}
		if !authFunc(credentialTag.Owner()) {
			results.Results[i].Error = common.ServerError(common.ErrPerm)
			continue
		}

		// Helper to look up and cache credential schemas for clouds.
		schemaCache := make(map[string]map[cloud.AuthType]cloud.CredentialSchema)
		credentialSchemas := func() (map[cloud.AuthType]cloud.CredentialSchema, error) {
			cloudName := credentialTag.Cloud().Id()
			if s, ok := schemaCache[cloudName]; ok {
				return s, nil
			}
			cloud, err := api.backend.Cloud(cloudName)
			if err != nil {
				return nil, err
			}
			provider, err := environs.Provider(cloud.Type)
			if err != nil {
				return nil, err
			}
			schema := provider.CredentialSchemas()
			schemaCache[cloudName] = schema
			return schema, nil
		}
		cloudCredentials, err := api.backend.CloudCredentials(credentialTag.Owner(), credentialTag.Cloud().Id())
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}

		cred, ok := cloudCredentials[credentialTag.Id()]
		if !ok {
			results.Results[i].Error = common.ServerError(errors.NotFoundf("credential %q", credentialTag.Name()))
			continue
		}

		schemas, err := credentialSchemas()
		if err != nil {
			results.Results[i].Error = common.ServerError(err)
			continue
		}

		attrs := cred.Attributes
		var redacted []string
		// Mask out the secrets.
		if s, ok := schemas[cloud.AuthType(cred.AuthType)]; ok {
			for _, attr := range s {
				if attr.Hidden {
					delete(attrs, attr.Name)
					redacted = append(redacted, attr.Name)
				}
			}
		}
		results.Results[i].Result = &params.CloudCredential{
			AuthType:   cred.AuthType,
			Attributes: attrs,
			Redacted:   redacted,
		}
	}
	return results, nil
}

// AddCloud adds a new cloud, different from the one managed by the controller.
func (api *CloudAPIV2) AddCloud(cloudArgs params.AddCloudArgs) error {
	err := api.backend.AddCloud(common.CloudFromParams(cloudArgs.Name, cloudArgs.Cloud))
	if err != nil {
		return err
	}
	return nil
}

// CredentialContents returns the specified cloud credentials,
// including the secrets if requested.
// If no specific credential name/cloud was passed in, all credentials for this user
// are returned.
// Only credential owner can see its contents as well as what models use it.
// Controller admin has no special superpowers here and is treated the same as all other users.
func (api *CloudAPIV2) CredentialContents(args params.CloudCredentialArgs) (params.CredentialContentResults, error) {
	// Helper to look up and cache credential schemas for clouds.
	schemaCache := make(map[string]map[cloud.AuthType]cloud.CredentialSchema)
	credentialSchemas := func(cloudName string) (map[cloud.AuthType]cloud.CredentialSchema, error) {
		if s, ok := schemaCache[cloudName]; ok {
			return s, nil
		}
		cloud, err := api.backend.Cloud(cloudName)
		if err != nil {
			return nil, err
		}
		provider, err := environs.Provider(cloud.Type)
		if err != nil {
			return nil, err
		}
		schema := provider.CredentialSchemas()
		schemaCache[cloudName] = schema
		return schema, nil
	}

	// Helper to parse state.Credential into an expected result item.
	stateIntoParam := func(credential state.Credential, includeSecrets bool) params.CredentialContentResult {
		schemas, err := credentialSchemas(credential.Cloud)
		if err != nil {
			return params.CredentialContentResult{Error: common.ServerError(err)}
		}
		attrs := map[string]string{}
		// Filter out the secrets.
		if s, ok := schemas[cloud.AuthType(credential.AuthType)]; ok {
			for _, attr := range s {
				if value, exists := credential.Attributes[attr.Name]; exists {
					if attr.Hidden && !includeSecrets {
						continue
					}
					attrs[attr.Name] = value
				}
			}
		}
		info := params.ControllerCredentialInfo{
			Content: params.CredentialContent{
				Name:       credential.Name,
				AuthType:   credential.AuthType,
				Attributes: attrs,
				Cloud:      credential.Cloud,
			},
		}

		// get models
		tag, err := credential.CloudCredentialTag()
		if err != nil {
			return params.CredentialContentResult{Error: common.ServerError(err)}
		}

		models, err := api.backend.CredentialModelsAndOwnerAccess(tag)
		if err != nil && !errors.IsNotFound(err) {
			return params.CredentialContentResult{Error: common.ServerError(err)}
		}
		info.Models = make([]params.ModelAccess, len(models))
		for i, m := range models {
			info.Models[i] = params.ModelAccess{m.ModelName, string(m.OwnerAccess)}
		}

		return params.CredentialContentResult{Result: &info}
	}

	var result []params.CredentialContentResult
	if len(args.Credentials) == 0 {
		credentials, err := api.backend.AllCloudCredentials(api.apiUser)
		if err != nil {
			return params.CredentialContentResults{}, errors.Trace(err)
		}
		result = make([]params.CredentialContentResult, len(credentials))
		for i, credential := range credentials {
			result[i] = stateIntoParam(credential, args.IncludeSecrets)
		}
	} else {
		// Helper to construct credential tag from cloud and name.
		credId := func(cloudName, credentialName string) string {
			return fmt.Sprintf("%s/%s/%s",
				cloudName, api.apiUser.Id(), credentialName,
			)
		}

		result = make([]params.CredentialContentResult, len(args.Credentials))
		for i, given := range args.Credentials {
			id := credId(given.CloudName, given.CredentialName)
			tag := names.NewCloudCredentialTag(id)
			credential, err := api.backend.CloudCredential(tag)
			if err != nil {
				result[i] = params.CredentialContentResult{
					Error: common.ServerError(err),
				}
				continue
			}
			result[i] = stateIntoParam(credential, args.IncludeSecrets)
		}
	}
	return params.CredentialContentResults{result}, nil
}
