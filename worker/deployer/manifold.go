// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package deployer

import (
	"github.com/juju/clock"
	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/names/v4"
	"github.com/juju/worker/v2"
	"github.com/juju/worker/v2/dependency"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/api/base"
	apideployer "github.com/juju/juju/api/deployer"
	"github.com/juju/juju/cmd/jujud/agent/engine"
)

// ManifoldConfig defines the names of the manifolds on which a Manifold will depend.
type ManifoldConfig struct {
	AgentName     string
	APICallerName string
	Clock         clock.Clock
	Logger        Logger

	UnitEngineConfig func() dependency.EngineConfig
	SetupLogging     func(*loggo.Context, agent.Config)
	NewDeployContext func(ContextConfig) (Context, error)
}

// TODO: add ManifoleConfig.Validate.

// Manifold returns a dependency manifold that runs a deployer worker,
// using the resource names defined in the supplied config.
func Manifold(config ManifoldConfig) dependency.Manifold {
	typedConfig := engine.AgentAPIManifoldConfig{
		AgentName:     config.AgentName,
		APICallerName: config.APICallerName,
	}
	return engine.AgentAPIManifold(typedConfig, config.newWorker)
}

// newWorker trivially wraps NewDeployer for use in a engine.AgentAPIManifold.
//
// It's not tested at the moment, because the scaffolding
// necessary is too unwieldy/distracting to introduce at this point.
func (config ManifoldConfig) newWorker(a agent.Agent, apiCaller base.APICaller) (worker.Worker, error) {
	// TODO: run config.Validate()
	cfg := a.CurrentConfig()
	// Grab the tag and ensure that it's for a machine.
	if cfg.Tag().Kind() != names.MachineTagKind {
		return nil, errors.New("agent's tag is not a machine tag")
	}
	deployerFacade := apideployer.NewState(apiCaller)
	contextConfig := ContextConfig{
		Agent:            a,
		Clock:            config.Clock,
		Logger:           config.Logger,
		UnitEngineConfig: config.UnitEngineConfig,
		SetupLogging:     config.SetupLogging,
		UnitManifolds:    UnitManifolds,
	}

	context, err := config.NewDeployContext(contextConfig)
	if err != nil {
		return nil, errors.Trace(err)
	}
	shim := &apiShim{deployerFacade}
	w, err := NewDeployer(shim, config.Logger, context)
	if err != nil {
		return nil, errors.Annotate(err, "cannot start unit agent deployer worker")
	}
	return w, nil
}

type apiShim struct {
	st *apideployer.State
}

func (s *apiShim) Machine(tag names.MachineTag) (Machine, error) {
	// Need to deal with typed nils.
	machine, err := s.st.Machine(tag)
	if err != nil {
		return nil, err
	}
	return machine, nil
}

func (s *apiShim) Unit(tag names.UnitTag) (Unit, error) {
	unit, err := s.st.Unit(tag)
	if err != nil {
		return nil, err
	}
	return unit, nil
}
