package plugin_manager

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/langgenius/dify-plugin-daemon/internal/core/dify_invocation"
	"github.com/langgenius/dify-plugin-daemon/internal/core/dify_invocation/real"
	"github.com/langgenius/dify-plugin-daemon/internal/core/plugin_manager/media_manager"
	"github.com/langgenius/dify-plugin-daemon/internal/core/plugin_manager/serverless"
	"github.com/langgenius/dify-plugin-daemon/internal/core/plugin_packager/decoder"
	"github.com/langgenius/dify-plugin-daemon/internal/types/app"
	"github.com/langgenius/dify-plugin-daemon/internal/types/entities/plugin_entities"
	"github.com/langgenius/dify-plugin-daemon/internal/utils/cache"
	"github.com/langgenius/dify-plugin-daemon/internal/utils/lock"
	"github.com/langgenius/dify-plugin-daemon/internal/utils/log"
	"github.com/langgenius/dify-plugin-daemon/internal/utils/mapping"
	"github.com/langgenius/dify-plugin-daemon/internal/utils/stream"
)

type PluginManager struct {
	m mapping.Map[string, plugin_entities.PluginLifetime]

	maxPluginPackageSize int64
	workingDirectory     string
	packageCachePath     string

	// mediaManager is used to manage media files like plugin icons, images, etc.
	mediaManager *media_manager.MediaManager

	// register plugin
	pluginRegisters []func(lifetime plugin_entities.PluginLifetime) error

	// running plugin in storage contains relations between plugin packages and their running instances
	runningPluginInStorage mapping.Map[string, string]
	// start process lock
	startProcessLock *lock.HighGranularityLock
	// serverless runtime

	// Install is a function that installs a plugin to the platform
	Install func(
		tenant_id string, decoder decoder.PluginDecoder,
		source string,
		meta map[string]any,
	) (*stream.Stream[PluginInstallResponse], error)

	// backwardsInvocation is a handle to invoke dify
	backwardsInvocation dify_invocation.BackwardsInvocation
}

var (
	manager *PluginManager
)

func NewManager(configuration *app.Config) *PluginManager {
	manager = &PluginManager{
		maxPluginPackageSize: configuration.MaxPluginPackageSize,
		packageCachePath:     configuration.PluginPackageCachePath,
		workingDirectory:     configuration.PluginWorkingPath,
		mediaManager: media_manager.NewMediaManager(
			configuration.PluginMediaCachePath,
			configuration.PluginMediaCacheSize,
		),
		startProcessLock: lock.NewHighGranularityLock(),
	}

	if configuration.Platform == app.PLATFORM_AWS_LAMBDA {
		manager.Install = manager.InstallToAWSFromPkg
		serverless.Init(configuration)
	} else if configuration.Platform == app.PLATFORM_LOCAL {
		manager.Install = manager.InstallToLocal
	}

	// mkdir
	os.MkdirAll(configuration.PluginWorkingPath, 0755)
	os.MkdirAll(configuration.PluginStoragePath, 0755)
	os.MkdirAll(configuration.PluginMediaCachePath, 0755)
	os.MkdirAll(configuration.PluginPackageCachePath, 0755)
	os.MkdirAll(filepath.Dir(configuration.ProcessCachingPath), 0755)

	return manager
}

func Manager() *PluginManager {
	return manager
}

func (p *PluginManager) Add(
	plugin plugin_entities.PluginLifetime,
) error {
	identity, err := plugin.Identity()
	if err != nil {
		return err
	}

	p.m.Store(identity.String(), plugin)
	return nil
}

func (p *PluginManager) Get(
	identity plugin_entities.PluginUniqueIdentifier,
) plugin_entities.PluginLifetime {
	if v, ok := p.m.Load(identity.String()); ok {
		return v
	}

	// check if plugin is a serverless runtime
	plugin_session_interface, err := p.getServerlessPluginRuntime(identity)
	if err != nil {
		return nil
	}

	return plugin_session_interface
}

func (p *PluginManager) GetAsset(id string) ([]byte, error) {
	return p.mediaManager.Get(id)
}

func (p *PluginManager) Init(configuration *app.Config) {
	log.Info("start plugin manager daemon...")

	// init redis client
	if err := cache.InitRedisClient(
		fmt.Sprintf("%s:%d", configuration.RedisHost, configuration.RedisPort),
		configuration.RedisPass,
	); err != nil {
		log.Panic("init redis client failed: %s", err.Error())
	}

	invocation, err := real.InitDifyInvocationDaemon(
		configuration.DifyInnerApiURL, configuration.DifyInnerApiKey,
	)
	if err != nil {
		log.Panic("init dify invocation daemon failed: %s", err.Error())
	}
	p.backwardsInvocation = invocation

	// start local watcher
	if configuration.Platform == app.PLATFORM_LOCAL {
		p.startLocalWatcher(configuration)
	}

	// start remote watcher
	p.startRemoteWatcher(configuration)
}

func (p *PluginManager) BackwardsInvocation() dify_invocation.BackwardsInvocation {
	return p.backwardsInvocation
}

func (p *PluginManager) SavePackage(plugin_unique_identifier plugin_entities.PluginUniqueIdentifier, pkg []byte) error {
	// save to storage
	pkg_path := filepath.Join(p.packageCachePath, plugin_unique_identifier.String())
	pkg_dir := filepath.Dir(pkg_path)
	if err := os.MkdirAll(pkg_dir, 0755); err != nil {
		return err
	}

	if err := os.WriteFile(pkg_path, pkg, 0644); err != nil {
		return err
	}

	return nil
}

func (p *PluginManager) GetPackage(plugin_unique_identifier plugin_entities.PluginUniqueIdentifier) ([]byte, error) {
	return os.ReadFile(filepath.Join(p.packageCachePath, plugin_unique_identifier.String()))
}
