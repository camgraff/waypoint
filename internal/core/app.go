package core

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mitchellh/devflow/internal/config"
	"github.com/mitchellh/devflow/internal/plugin"
	pb "github.com/mitchellh/devflow/internal/server/gen"
	"github.com/mitchellh/devflow/internal/serverhistory"
	"github.com/mitchellh/devflow/sdk/component"
	"github.com/mitchellh/devflow/sdk/datadir"
	"github.com/mitchellh/devflow/sdk/internal-shared/mapper"
	"github.com/mitchellh/devflow/sdk/terminal"
)

// App represents a single application and exposes all the operations
// that can be performed on an application.
//
// An App is only valid if it was returned by Project.App. The behavior of
// App if constructed in any other way is undefined and likely to result
// in crashes.
type App struct {
	Builder  component.Builder
	Registry component.Registry
	Platform component.Platform
	Releaser component.ReleaseManager

	// UI is the UI that should be used for any output that is specific
	// to this app vs the project UI.
	UI terminal.UI

	client        pb.DevflowClient
	source        *component.Source
	dconfig       component.DeploymentConfig
	logger        hclog.Logger
	dir           *datadir.App
	mappers       []*mapper.Func
	components    map[interface{}]*pb.Component
	componentDirs map[interface{}]*datadir.Component
	closers       []func() error
}

// newApp creates an App for the given project and configuration. This will
// initialize and configure all the components of this application. An error
// will be returned if this app fails to initialize: configuration is invalid,
// a component could not be found, etc.
func newApp(ctx context.Context, p *Project, cfg *config.App) (*App, error) {
	// Initialize
	app := &App{
		client:        p.client,
		source:        &component.Source{App: cfg.Name, Path: "."},
		dconfig:       p.dconfig,
		logger:        p.logger.Named("app").Named(cfg.Name),
		components:    make(map[interface{}]*pb.Component),
		componentDirs: make(map[interface{}]*datadir.Component),

		// very important below that we allocate a new slice since we modify
		mappers: append([]*mapper.Func{}, p.mappers...),

		// set the UI, which for now is identical to project but in the
		// future should probably change as we do app-scoping, parallelization,
		// etc.
		UI: p.UI,
	}

	// Setup our directory
	dir, err := p.dir.App(cfg.Name)
	if err != nil {
		return nil, err
	}
	app.dir = dir

	// Load all the components
	components := []struct {
		Target interface{}
		Type   component.Type
		Config *config.Component
	}{
		{&app.Builder, component.BuilderType, cfg.Build},
		{&app.Registry, component.RegistryType, cfg.Registry},
		{&app.Platform, component.PlatformType, cfg.Platform},
		{&app.Releaser, component.ReleaseManagerType, cfg.Release},
	}
	for _, c := range components {
		if c.Config == nil {
			// This component is not set, ignore.
			continue
		}

		err = app.initComponent(ctx, c.Type, c.Target, p.factories[c.Type], c.Config)
		if err != nil {
			return nil, err
		}
	}

	return app, nil
}

// Close is called to clean up any resources. This should be called
// whenever the app is done being used. This will be called by Project.Close.
func (a *App) Close() error {
	for _, c := range a.closers {
		c()
	}

	return nil
}

// Exec using the deployer phase
// TODO(evanphx): test
func (a *App) Exec(ctx context.Context) error {
	log := a.logger.Named("platform")

	ep, ok := a.Platform.(component.ExecPlatform)
	if !ok {
		return fmt.Errorf("This platform does not support exec yet")
	}

	_, err := a.callDynamicFunc(ctx, log, nil, a.Platform, ep.ExecFunc())
	if err != nil {
		return err
	}

	return nil
}

// Set config variables on the deployer phase
// TODO(evanphx): test
func (a *App) ConfigSet(ctx context.Context, key, val string) error {
	log := a.logger.Named("platform")

	ep, ok := a.Platform.(component.ConfigPlatform)
	if !ok {
		return fmt.Errorf("This platform does not support config yet")
	}

	cv := &component.ConfigVar{Name: key, Value: val}

	_, err := a.callDynamicFunc(ctx, log, nil, a.Platform, ep.ConfigSetFunc(), cv)
	if err != nil {
		return err
	}

	return nil
}

// Get config variables on the deployer phase
// TODO(evanphx): test
func (a *App) ConfigGet(ctx context.Context, key string) (*component.ConfigVar, error) {
	log := a.logger.Named("platform")

	ep, ok := a.Platform.(component.ConfigPlatform)
	if !ok {
		return nil, fmt.Errorf("This platform does not support config yet")
	}

	cv := &component.ConfigVar{
		Name: key,
	}

	_, err := a.callDynamicFunc(ctx, log, nil, a.Platform, ep.ConfigGetFunc(), cv)
	if err != nil {
		return nil, err
	}

	return cv, nil
}

// callDynamicFunc calls a dynamic function which is a common pattern for
// our component interfaces. These are functions that are given to mapper,
// supplied with a series of arguments, dependency-injected, and then called.
//
// This always provides some common values for injection:
//
//   * *component.Source
//   * *datadir.Project
//   * history.Client
//
func (a *App) callDynamicFunc(
	ctx context.Context,
	log hclog.Logger,
	result interface{}, // expected result type
	c interface{}, // component
	f interface{}, // function
	values ...interface{},
) (interface{}, error) {
	// We allow f to be a *mapper.Func because our plugin system creates
	// a func directly due to special argument types.
	// TODO: test
	rawFunc, ok := f.(*mapper.Func)
	if !ok {
		var err error
		rawFunc, err = mapper.NewFunc(f, mapper.WithLogger(log))
		if err != nil {
			return nil, err
		}
	}

	// Get the component directory
	cdir, ok := a.componentDirs[c]
	if !ok {
		return nil, fmt.Errorf("component dir not found for: %T", c)
	}

	// Make sure we have access to our context and logger and default args
	values = append(values,
		ctx,
		log,
		a.source,
		a.dir,
		cdir,
		a.UI,
		&serverhistory.Client{APIClient: a.client, MapperSet: mapper.Set(a.mappers)},
	)

	// Build the chain and call it
	chain, err := rawFunc.Chain(a.mappers, values...)
	if err != nil {
		return nil, err
	}
	log.Debug("function chain", "chain", chain.String())
	raw, err := chain.Call()
	if err != nil {
		return nil, err
	}

	// If we don't have an expected result type, then just return as-is.
	// Otherwise, we need to verify the result type matches properly.
	if result == nil {
		return raw, nil
	}

	// Verify
	interfaceType := reflect.TypeOf(result).Elem()
	if rawType := reflect.TypeOf(raw); !rawType.Implements(interfaceType) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"operation expected result type %s, got %s",
			interfaceType.String(),
			rawType.String())
	}

	return raw, nil
}

// initComponent initializes a component with the given factory and configuration
// and then sets it on the value pointed to by target.
func (a *App) initComponent(
	ctx context.Context,
	typ component.Type,
	target interface{},
	f *mapper.Factory,
	cfg *config.Component,
) error {
	log := a.logger.Named(strings.ToLower(typ.String()))

	// Before we do anything, the target should be a pointer. If so,
	// then we get the value of the pointer so we can set it later.
	targetV := reflect.ValueOf(target)
	if targetV.Kind() != reflect.Ptr {
		return fmt.Errorf("target value should be a pointer")
	}
	targetV = reflect.Indirect(targetV)

	// Get the factory function for this type
	fn := f.Func(cfg.Type)
	if fn == nil {
		return fmt.Errorf("unknown type: %q", cfg.Type)
	}

	// Create the data directory for this component
	cdir, err := a.dir.Component(strings.ToLower(typ.String()), cfg.Type)
	if err != nil {
		return err
	}

	// Call the factory to get our raw value (interface{} type)
	raw, err := fn.Call(ctx, a.source, log, cdir)
	if err != nil {
		return err
	}
	log.Info("initialized component", "type", typ.String())

	// If we have a plugin.Instance then we can extract other information
	// from this plugin. We accept pure factories too that don't return
	// this so we type-check here.
	if pinst, ok := raw.(*plugin.Instance); ok {
		raw = pinst.Component

		// Plugins may contain their own dedicated mappers. We want to be
		// aware of them so that we can map data to/from as necessary.
		// These mappers become app-specific here so that other apps aren't
		// affected by other plugins.
		a.mappers = append(a.mappers, pinst.Mappers...)
		log.Info("registered component-specific mappers", "len", len(pinst.Mappers))

		// Store the closer
		a.closers = append(a.closers, func() error {
			pinst.Close()
			return nil
		})
	}

	// Store the component dir mapping
	a.componentDirs[raw] = cdir

	// We have our value so let's make sure it is the correct type.
	rawV := reflect.ValueOf(raw)
	if !rawV.Type().AssignableTo(targetV.Type()) {
		return fmt.Errorf("component %s not assigntable to type %s", rawV.Type(), targetV.Type())
	}

	// Configure the component. This will handle all the cases where no
	// config is given but required, vice versa, and everything in between.
	diag := component.Configure(raw, cfg.Body, nil)
	if diag.HasErrors() {
		return diag
	}

	// Assign our value now that we won't error anymore
	targetV.Set(rawV)

	// Store component metadata
	a.components[raw] = &pb.Component{
		Type: pb.Component_Type(typ),
		Name: cfg.Type,
	}

	return nil
}
