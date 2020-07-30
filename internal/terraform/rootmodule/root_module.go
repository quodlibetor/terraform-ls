package rootmodule

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	"github.com/hashicorp/terraform-ls/internal/terraform/addrs"
	"github.com/hashicorp/terraform-ls/internal/terraform/discovery"
	"github.com/hashicorp/terraform-ls/internal/terraform/exec"
	"github.com/hashicorp/terraform-ls/internal/terraform/lang"
	"github.com/hashicorp/terraform-ls/internal/terraform/schema"
)

type rootModule struct {
	path   string
	logger *log.Logger

	// loading
	isLoading     bool
	isLoadingMu   *sync.RWMutex
	loadingDone   <-chan struct{}
	cancelLoading context.CancelFunc
	loadErr       error
	loadErrMu     *sync.RWMutex

	// module cache
	moduleMu           *sync.RWMutex
	moduleManifestFile File
	moduleManifest     *moduleManifest

	// plugin cache
	pluginMu         *sync.RWMutex
	pluginLockFile   File
	newSchemaStorage schema.StorageFactory
	schemaStorage    *schema.Storage
	schemaLoaded     bool
	schemaLoadedMu   *sync.RWMutex

	// terraform executor
	tfLoaded      bool
	tfLoadedMu    *sync.RWMutex
	tfExec        *exec.Executor
	tfNewExecutor exec.ExecutorFactory
	tfExecPath    string
	tfExecTimeout time.Duration
	tfExecLogPath string

	// terraform discovery
	tfDiscoFunc  discovery.DiscoveryFunc
	tfDiscoErr   error
	tfVersion    string
	tfVersionErr error

	// language parser
	parserLoaded bool
	parserMu     *sync.RWMutex
	parser       lang.Parser

	// provider references
	providerRefs   addrs.ProviderReferences
	providerRefsMu *sync.RWMutex
}

func newRootModule(dir string) *rootModule {
	return &rootModule{
		path:           dir,
		logger:         defaultLogger,
		providerRefs:   make(addrs.ProviderReferences, 0),
		providerRefsMu: &sync.RWMutex{},
		isLoadingMu:    &sync.RWMutex{},
		loadErrMu:      &sync.RWMutex{},
		moduleMu:       &sync.RWMutex{},
		pluginMu:       &sync.RWMutex{},
		schemaLoadedMu: &sync.RWMutex{},
		tfLoadedMu:     &sync.RWMutex{},
		parserMu:       &sync.RWMutex{},
	}
}

var defaultLogger = log.New(ioutil.Discard, "", 0)

func NewRootModule(ctx context.Context, dir string) (RootModule, error) {
	rm := newRootModule(dir)

	d := &discovery.Discovery{}
	rm.tfDiscoFunc = d.LookPath

	rm.tfNewExecutor = exec.NewExecutor
	rm.newSchemaStorage = schema.NewStorageForVersion

	err := rm.discoverCaches(ctx, dir)
	if err != nil {
		return rm, err
	}

	return rm, rm.load(ctx)
}

func (rm *rootModule) discoverCaches(ctx context.Context, dir string) error {
	var errs *multierror.Error
	err := rm.discoverPluginCache(dir)
	if err != nil {
		errs = multierror.Append(errs, err)
	}

	err = rm.discoverModuleCache(dir)
	if err != nil {
		errs = multierror.Append(errs, err)
	}

	return errs.ErrorOrNil()
}

func (rm *rootModule) discoverPluginCache(dir string) error {
	rm.pluginMu.Lock()
	defer rm.pluginMu.Unlock()

	lockPaths := pluginLockFilePaths(dir)
	lf, err := findFile(lockPaths)
	if err != nil {
		if os.IsNotExist(err) {
			rm.logger.Printf("no plugin cache found: %s", err.Error())
			return nil
		}

		return fmt.Errorf("unable to calculate hash: %w", err)
	}
	rm.pluginLockFile = lf
	return nil
}

func (rm *rootModule) discoverModuleCache(dir string) error {
	rm.moduleMu.Lock()
	defer rm.moduleMu.Unlock()

	lf, err := newFile(moduleManifestFilePath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			rm.logger.Printf("no module manifest file found: %s", err.Error())
			return nil
		}

		return fmt.Errorf("unable to calculate hash: %w", err)
	}
	rm.moduleManifestFile = lf
	return nil
}

func (rm *rootModule) Modules() []ModuleRecord {
	rm.moduleMu.Lock()
	defer rm.moduleMu.Unlock()
	if rm.moduleManifest == nil {
		return []ModuleRecord{}
	}

	return rm.moduleManifest.Records
}

func (rm *rootModule) SetLogger(logger *log.Logger) {
	rm.logger = logger
}

func (rm *rootModule) StartLoading() error {
	if !rm.IsLoadingDone() {
		return fmt.Errorf("root module is already being loaded")
	}
	ctx, cancelFunc := context.WithCancel(context.Background())
	rm.cancelLoading = cancelFunc
	rm.loadingDone = ctx.Done()

	go func(ctx context.Context) {
		rm.setLoadErr(rm.load(ctx))
	}(ctx)
	return nil
}

func (rm *rootModule) CancelLoading() {
	if !rm.IsLoadingDone() && rm.cancelLoading != nil {
		rm.cancelLoading()
	}
	rm.setLoadingState(false)
}

func (rm *rootModule) LoadingDone() <-chan struct{} {
	return rm.loadingDone
}

func (rm *rootModule) load(ctx context.Context) error {
	var errs *multierror.Error
	defer rm.CancelLoading()

	// reset internal loading state
	rm.setLoadingState(true)

	// The following operations have to happen in a particular order
	// as they depend on the internal state as mutated by each operation

	err := rm.UpdateModuleManifest(rm.moduleManifestFile)
	errs = multierror.Append(errs, err)

	err = rm.discoverTerraformExecutor(ctx)
	rm.tfDiscoErr = err
	errs = multierror.Append(errs, err)

	err = rm.discoverTerraformVersion(ctx)
	rm.tfVersionErr = err
	errs = multierror.Append(errs, err)

	err = rm.ParseProviderReferences()
	errs = multierror.Append(errs, err)

	err = rm.findCompatibleLangParser()
	errs = multierror.Append(errs, err)

	err = rm.findCompatibleStateStorage()
	errs = multierror.Append(errs, err)

	err = rm.UpdateSchemaCache(ctx, rm.pluginLockFile)
	errs = multierror.Append(errs, err)

	rm.logger.Printf("loading of root module %s finished: %s",
		rm.Path(), errs)
	return errs.ErrorOrNil()
}

func (rm *rootModule) setLoadingState(isLoading bool) {
	rm.isLoadingMu.Lock()
	defer rm.isLoadingMu.Unlock()
	rm.isLoading = isLoading
}

func (rm *rootModule) IsLoadingDone() bool {
	rm.isLoadingMu.RLock()
	defer rm.isLoadingMu.RUnlock()
	return !rm.isLoading
}

func (rm *rootModule) discoverTerraformExecutor(ctx context.Context) error {
	defer func() {
		rm.setTfLoaded(true)
	}()

	tfPath := rm.tfExecPath
	if tfPath == "" {
		var err error
		tfPath, err = rm.tfDiscoFunc()
		if err != nil {
			return err
		}
	}

	tf := rm.tfNewExecutor(tfPath)

	tf.SetWorkdir(rm.path)
	tf.SetLogger(rm.logger)

	if rm.tfExecLogPath != "" {
		tf.SetExecLogPath(rm.tfExecLogPath)
	}

	if rm.tfExecTimeout != 0 {
		tf.SetTimeout(rm.tfExecTimeout)
	}

	rm.tfExec = tf

	return nil
}

func (rm *rootModule) discoverTerraformVersion(ctx context.Context) error {
	if rm.tfExec == nil {
		return errors.New("no terraform executor - unable to read version")
	}

	version, err := rm.tfExec.Version(ctx)
	if err != nil {
		return err
	}
	rm.logger.Printf("Terraform version %s found at %s for %s", version,
		rm.tfExec.GetExecPath(), rm.Path())
	rm.tfVersion = version
	return nil
}

func (rm *rootModule) findCompatibleStateStorage() error {
	if rm.tfVersion == "" {
		return errors.New("unknown terraform version - unable to find state storage")
	}

	ss, err := rm.newSchemaStorage(rm.tfVersion)
	if err != nil {
		return err
	}
	rm.schemaStorage = ss
	rm.schemaStorage.SetLogger(rm.logger)

	if rm.IsParserLoaded() {
		rm.parser.SetSchemaReader(rm.schemaStorage)
	}

	return nil
}

func (rm *rootModule) findCompatibleLangParser() error {
	defer func() {
		rm.setParserLoaded(true)
	}()

	if rm.tfVersion == "" {
		return errors.New("unknown terraform version - unable to find parser")
	}

	p, err := lang.FindCompatibleParser(rm.tfVersion)
	if err != nil {
		return err
	}
	p.SetLogger(rm.logger)

	rm.parser = p

	return nil
}

func (rm *rootModule) ParseProviderReferences() error {
	rm.providerRefsMu.Lock()
	defer rm.providerRefsMu.Unlock()

	mod, diags := tfconfig.LoadModuleFromFilesystem(rm.filesystem, rm.Path())
	if diags.HasErrors() {
		rm.logger.Printf("parsing provider references for %s failed: %s",
			rm.Path(), diags.Error())
	}
	if mod == nil {
		rm.logger.Printf("no provider references parsed for %s", rm.Path())
		return nil
	}

	refs := make(addrs.ProviderReferences, 0)

	rm.logger.Printf("%d provider references found for %s",
		len(mod.RequiredProviders), rm.Path())

	for name, rp := range mod.RequiredProviders {
		if name == "" {
			// skip unnamed inferred provider references
			continue
		}

		lName, err := addrs.ParseProviderConfigCompactStr(name)
		if err != nil {
			return err
		}
		if rp.Source != "" {
			pAddr, err := addrs.ParseProviderSourceString(rp.Source)
			if err != nil {
				return err
			}
			refs[lName] = pAddr
		}
	}

	rm.providerRefs = refs

	if rm.IsParserLoaded() {
		rm.parserMu.Lock()
		defer rm.parserMu.Unlock()
		rm.parser.SetProviderReferences(rm.providerRefs)
	}

	return nil
}

func (rm *rootModule) LoadError() error {
	rm.loadErrMu.RLock()
	defer rm.loadErrMu.RUnlock()
	return rm.loadErr
}

func (rm *rootModule) setLoadErr(err error) {
	rm.loadErrMu.Lock()
	defer rm.loadErrMu.Unlock()
	rm.loadErr = err
}

func (rm *rootModule) Path() string {
	return rm.path
}

func (rm *rootModule) UpdateModuleManifest(lockFile File) error {
	rm.moduleMu.Lock()
	defer rm.moduleMu.Unlock()

	if lockFile == nil {
		rm.logger.Printf("ignoring module update as no lock file was found for %s", rm.Path())
		return nil
	}

	rm.moduleManifestFile = lockFile

	mm, err := ParseModuleManifestFromFile(lockFile.Path())
	if err != nil {
		return fmt.Errorf("failed to update module manifest: %w", err)
	}

	rm.moduleManifest = mm
	rm.logger.Printf("updated module manifest - %d references parsed for %s",
		len(mm.Records), rm.Path())
	return nil
}

func (rm *rootModule) Parser() (lang.Parser, error) {
	if !rm.IsParserLoaded() {
		return nil, fmt.Errorf("parser is not loaded yet")
	}
	rm.parserMu.RLock()
	defer rm.parserMu.RUnlock()

	if rm.parser == nil {
		return nil, fmt.Errorf("no parser available")
	}

	return rm.parser, nil
}

func (rm *rootModule) IsParserLoaded() bool {
	rm.parserMu.RLock()
	defer rm.parserMu.RUnlock()
	return rm.parserLoaded
}

func (rm *rootModule) setParserLoaded(isLoaded bool) {
	rm.parserMu.Lock()
	defer rm.parserMu.Unlock()
	rm.parserLoaded = isLoaded
}

func (rm *rootModule) IsSchemaLoaded() bool {
	rm.schemaLoadedMu.RLock()
	defer rm.schemaLoadedMu.RUnlock()
	return rm.schemaLoaded
}

func (rm *rootModule) setSchemaLoaded(isLoaded bool) {
	rm.schemaLoadedMu.Lock()
	defer rm.schemaLoadedMu.Unlock()
	rm.schemaLoaded = isLoaded
}

func (rm *rootModule) ReferencesModulePath(path string) bool {
	rm.moduleMu.Lock()
	defer rm.moduleMu.Unlock()
	if rm.moduleManifest == nil {
		return false
	}

	for _, m := range rm.moduleManifest.Records {
		if m.IsRoot() {
			// skip root module, as that's tracked separately
			continue
		}
		if m.IsExternal() {
			// skip external modules as these shouldn't be modified from cache
			continue
		}
		absPath := filepath.Join(rm.moduleManifest.rootDir, m.Dir)
		rm.logger.Printf("checking if %q equals %q", absPath, path)
		if pathEquals(absPath, path) {
			return true
		}
	}

	return false
}

func (rm *rootModule) TerraformFormatter() (exec.Formatter, error) {
	if !rm.IsTerraformLoaded() {
		return nil, fmt.Errorf("terraform executor is not loaded yet")
	}

	if rm.tfExec == nil {
		return nil, fmt.Errorf("no terraform executor available")
	}

	return rm.tfExec.FormatterForVersion(rm.tfVersion)
}

func (rm *rootModule) IsTerraformLoaded() bool {
	rm.tfLoadedMu.RLock()
	defer rm.tfLoadedMu.RUnlock()
	return rm.tfLoaded
}

func (rm *rootModule) setTfLoaded(isLoaded bool) {
	rm.tfLoadedMu.Lock()
	defer rm.tfLoadedMu.Unlock()
	rm.tfLoaded = isLoaded
}

func (rm *rootModule) UpdateSchemaCache(ctx context.Context, lockFile File) error {
	rm.pluginMu.Lock()
	defer rm.pluginMu.Unlock()

	if !rm.IsTerraformLoaded() {
		return fmt.Errorf("cannot update schema as terraform executor is not available yet")
	}

	defer func() {
		rm.setSchemaLoaded(true)
	}()

	if lockFile == nil {
		rm.logger.Printf("ignoring schema cache update as no lock file was found for %s",
			rm.Path())
		return nil
	}

	if rm.schemaStorage == nil {
		return fmt.Errorf("cannot update schema as schema cache is not available")
	}

	rm.pluginLockFile = lockFile

	return rm.schemaStorage.ObtainSchemasForModule(ctx,
		rm.tfExec, rootModuleDirFromFilePath(lockFile.Path()))
}

func (rm *rootModule) PathsToWatch() []string {
	rm.pluginMu.RLock()
	rm.moduleMu.RLock()
	defer rm.moduleMu.RUnlock()
	defer rm.pluginMu.RUnlock()

	files := make([]string, 0)
	if rm.pluginLockFile != nil {
		files = append(files, rm.pluginLockFile.Path())
	}
	if rm.moduleManifestFile != nil {
		files = append(files, rm.moduleManifestFile.Path())
	}

	return files
}

func (rm *rootModule) IsKnownModuleManifestFile(path string) bool {
	rm.moduleMu.RLock()
	defer rm.moduleMu.RUnlock()

	if rm.moduleManifestFile == nil {
		return false
	}

	return pathEquals(rm.moduleManifestFile.Path(), path)
}

func (rm *rootModule) IsKnownPluginLockFile(path string) bool {
	rm.pluginMu.RLock()
	defer rm.pluginMu.RUnlock()

	if rm.pluginLockFile == nil {
		return false
	}

	return pathEquals(rm.pluginLockFile.Path(), path)
}
