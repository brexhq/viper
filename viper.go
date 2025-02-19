// Copyright © 2014 Steve Francia <spf@spf13.com>.
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// Viper is an application configuration system.
// It believes that applications can be configured a variety of ways
// via flags, ENVIRONMENT variables, configuration files retrieved
// from the file system, or a remote key/value store.

// Each item takes precedence over the item below it:

// overrides
// flag
// env
// config
// key/value store
// default

package viper

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/ristretto"

	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/hcl"
	"github.com/hashicorp/hcl/hcl/printer"
	"github.com/magiconair/properties"
	"github.com/mitchellh/mapstructure"
	"github.com/pelletier/go-toml"
	"github.com/spf13/afero"
	"github.com/spf13/cast"
	jww "github.com/spf13/jwalterweatherman"
	"github.com/spf13/pflag"
	"github.com/subosito/gotenv"
	"gopkg.in/ini.v1"
	"gopkg.in/yaml.v2"
)

// ConfigMarshalError happens when failing to marshal the configuration.
type ConfigMarshalError struct {
	err error
}

// Error returns the formatted configuration error.
func (e ConfigMarshalError) Error() string {
	return fmt.Sprintf("While marshaling config: %s", e.err.Error())
}

var v *Viper

type RemoteResponse struct {
	Value []byte
	Error error
}

func init() {
	v = New()
}

type remoteConfigFactory interface {
	Get(rp RemoteProvider) (io.Reader, error)
	Watch(rp RemoteProvider) (io.Reader, error)
	WatchChannel(rp RemoteProvider) (<-chan *RemoteResponse, chan bool)
}

// RemoteConfig is optional, see the remote package
var RemoteConfig remoteConfigFactory

// UnsupportedConfigError denotes encountering an unsupported
// configuration filetype.
type UnsupportedConfigError string

// Error returns the formatted configuration error.
func (str UnsupportedConfigError) Error() string {
	return fmt.Sprintf("Unsupported Config Type %q", string(str))
}

// UnsupportedRemoteProviderError denotes encountering an unsupported remote
// provider. Currently only etcd and Consul are supported.
type UnsupportedRemoteProviderError string

// Error returns the formatted remote provider error.
func (str UnsupportedRemoteProviderError) Error() string {
	return fmt.Sprintf("Unsupported Remote Provider Type %q", string(str))
}

// RemoteConfigError denotes encountering an error while trying to
// pull the configuration from the remote provider.
type RemoteConfigError string

// Error returns the formatted remote provider error
func (rce RemoteConfigError) Error() string {
	return fmt.Sprintf("Remote Configurations Error: %s", string(rce))
}

// ConfigFileNotFoundError denotes failing to find configuration file.
type ConfigFileNotFoundError struct {
	name, locations string
}

// Error returns the formatted configuration error.
func (fnfe ConfigFileNotFoundError) Error() string {
	return fmt.Sprintf("Config File %q Not Found in %q", fnfe.name, fnfe.locations)
}

// ConfigFileAlreadyExistsError denotes failure to write new configuration file.
type ConfigFileAlreadyExistsError string

// Error returns the formatted error when configuration already exists.
func (faee ConfigFileAlreadyExistsError) Error() string {
	return fmt.Sprintf("Config File %q Already Exists", string(faee))
}

// A DecoderConfigOption can be passed to viper.Unmarshal to configure
// mapstructure.DecoderConfig options
type DecoderConfigOption func(*mapstructure.DecoderConfig)

// DecodeHook returns a DecoderConfigOption which overrides the default
// DecoderConfig.DecodeHook value, the default is:
//
//	 mapstructure.ComposeDecodeHookFunc(
//			mapstructure.StringToTimeDurationHookFunc(),
//			mapstructure.StringToSliceHookFunc(","),
//		)
func DecodeHook(hook mapstructure.DecodeHookFunc) DecoderConfigOption {
	return func(c *mapstructure.DecoderConfig) {
		c.DecodeHook = hook
	}
}

// Viper is a prioritized configuration registry. It
// maintains a set of configuration sources, fetches
// values to populate those, and provides them according
// to the source's priority.
// The priority of the sources is the following:
// 1. overrides
// 2. flags
// 3. env. variables
// 4. config file
// 5. key/value store
// 6. defaults
//
// For example, if values from the following sources were loaded:
//
//	Defaults : {
//		"secret": "",
//		"user": "default",
//		"endpoint": "https://localhost"
//	}
//	Config : {
//		"user": "root"
//		"secret": "defaultsecret"
//	}
//	Env : {
//		"secret": "somesecretkey"
//	}
//
// The resulting config will have the following values:
//
//	{
//		"secret": "somesecretkey",
//		"user": "root",
//		"endpoint": "https://localhost"
//	}
type Viper struct {
	// Delimiter that separates a list of keys
	// used to access a nested value in one go
	keyDelim string

	// A set of paths to look for the config file in
	configPaths []string

	// The filesystem to read config from.
	fs afero.Fs

	// A set of remote providers to search for the configuration
	remoteProviders []*defaultRemoteProvider

	// Name of file to look for inside the path
	configName        string
	configFile        string
	configType        string
	configPermissions os.FileMode
	configChangedAt   time.Time
	envPrefix         string

	automaticEnvApplied bool
	envKeyReplacer      StringReplacer
	allowEmptyEnv       bool

	config         map[string]interface{}
	override       map[string]interface{}
	defaults       map[string]interface{}
	types          map[string]interface{}
	kvstore        map[string]interface{}
	pflags         map[string]FlagValue
	env            map[string]string
	aliases        map[string]string
	typeByDefValue bool

	previousValues map[string]interface{}

	// Store read properties on the object so that we can write back in order with comments.
	// This will only be used if the configuration read is a properties file.
	properties *properties.Properties

	onConfigChange func(fsnotify.Event)

	cache        *ristretto.Cache[string, interface{}]
	cacheMaxCost int64

	lock *sync.RWMutex
}

// New returns an initialized Viper instance.
func New() *Viper {
	v := new(Viper)
	v.keyDelim = "."
	v.configName = "config"
	v.configChangedAt = time.Now()
	v.configPermissions = os.FileMode(0644)
	v.fs = afero.NewOsFs()
	v.config = make(map[string]interface{})
	v.override = make(map[string]interface{})
	v.defaults = make(map[string]interface{})
	v.types = make(map[string]interface{})
	v.kvstore = make(map[string]interface{})
	v.previousValues = make(map[string]interface{})
	v.pflags = make(map[string]FlagValue)
	v.env = make(map[string]string)
	v.aliases = make(map[string]string)
	v.typeByDefValue = false
	// v.lock = &lockLogger{new(sync.RWMutex)}
	v.lock = new(sync.RWMutex)

	var err error
	v.cacheMaxCost = 1 << 20 // 1MB max cache
	v.cache, err = ristretto.NewCache(&ristretto.Config[string, interface{}]{
		NumCounters: 1000,
		MaxCost:     1 << 20,
		BufferItems: 64,
	})
	if err != nil {
		// This will only happen if ristretto decides to throw an error based on the given configuration
		// in future versions which is unlikely and therefore a panic'able error.
		//
		// Currently, ristretto will only error if one of the provided settings (NumCounters, MaxCost, BufferItems)
		// is <= 0.
		panic(fmt.Sprintf("cache options are invalid because: %s", err))
	}

	return v
}

// Option configures Viper using the functional options paradigm popularized by Rob Pike and Dave Cheney.
// If you're unfamiliar with this style,
// see https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html and
// https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis.
type Option interface {
	apply(v *Viper)
}

type optionFunc func(v *Viper)

func (fn optionFunc) apply(v *Viper) {
	fn(v)
}

// KeyDelimiter sets the delimiter used for determining key parts.
// By default it's value is ".".
func KeyDelimiter(d string) Option {
	return optionFunc(func(v *Viper) {
		v.keyDelim = d
	})
}

// StringReplacer applies a set of replacements to a string.
type StringReplacer interface {
	// Replace returns a copy of s with all replacements performed.
	Replace(s string) string
}

// EnvKeyReplacer sets a replacer used for mapping environment variables to internal keys.
func EnvKeyReplacer(r StringReplacer) Option {
	return optionFunc(func(v *Viper) {
		v.envKeyReplacer = r
	})
}

// Cache sets Viper's cache (*ristretto.Cache). You must also pass the ristretto.Config
// object for some internal processing.
func Cache(c *ristretto.Cache[string, interface{}], cf *ristretto.Config[string, []byte]) Option {
	return optionFunc(func(v *Viper) {
		v.cache = c
		v.cacheMaxCost = cf.MaxCost
	})
}

// NewWithOptions creates a new Viper instance.
func NewWithOptions(opts ...Option) *Viper {
	v := New()

	for _, opt := range opts {
		opt.apply(v)
	}

	return v
}

// Reset is intended for testing, will reset all to default settings.
// In the public interface for the viper package so applications
// can use it in their testing as well.
func Reset() {
	v = New()
	SupportedExts = []string{"json", "toml", "yaml", "yml", "properties", "props", "prop", "hcl", "dotenv", "env", "ini"}
	SupportedRemoteProviders = []string{"etcd", "consul", "firestore"}
}

type defaultRemoteProvider struct {
	provider      string
	endpoint      string
	path          string
	secretKeyring string
}

func (rp defaultRemoteProvider) Provider() string {
	return rp.provider
}

func (rp defaultRemoteProvider) Endpoint() string {
	return rp.endpoint
}

func (rp defaultRemoteProvider) Path() string {
	return rp.path
}

func (rp defaultRemoteProvider) SecretKeyring() string {
	return rp.secretKeyring
}

// RemoteProvider stores the configuration necessary
// to connect to a remote key/value store.
// Optional secretKeyring to unencrypt encrypted values
// can be provided.
type RemoteProvider interface {
	Provider() string
	Endpoint() string
	Path() string
	SecretKeyring() string
}

// SupportedExts are universally supported extensions.
var SupportedExts = []string{"json", "toml", "yaml", "yml", "properties", "props", "prop", "hcl", "dotenv", "env", "ini"}

// SupportedRemoteProviders are universally supported remote providers.
var SupportedRemoteProviders = []string{"etcd", "consul", "firestore"}

func OnConfigChange(run func(in fsnotify.Event)) { v.OnConfigChange(run) }
func (v *Viper) OnConfigChange(run func(in fsnotify.Event)) {
	v.onConfigChange = run
}

func WatchConfig() { v.WatchConfig() }

func (v *Viper) WatchConfig() {
	initWG := sync.WaitGroup{}
	initWG.Add(1)
	go func() {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Fatal(err)
		}
		defer watcher.Close()
		// we have to watch the entire directory to pick up renames/atomic saves in a cross-platform way
		filename, err := v.getConfigFile()
		if err != nil {
			log.Printf("error: %v\n", err)
			initWG.Done()
			return
		}

		configFile := filepath.Clean(filename)
		configDir, _ := filepath.Split(configFile)
		realConfigFile, _ := filepath.EvalSymlinks(filename)

		eventsWG := sync.WaitGroup{}
		eventsWG.Add(1)
		go func() {
			for {
				select {
				case event, ok := <-watcher.Events:
					if !ok { // 'Events' channel is closed
						eventsWG.Done()
						return
					}
					currentConfigFile, _ := filepath.EvalSymlinks(filename)
					// we only care about the config file with the following cases:
					// 1 - if the config file was modified or created
					// 2 - if the real path to the config file changed (eg: k8s ConfigMap replacement)
					const writeOrCreateMask = fsnotify.Write | fsnotify.Create
					if (filepath.Clean(event.Name) == configFile &&
						event.Op&writeOrCreateMask != 0) ||
						(currentConfigFile != "" && currentConfigFile != realConfigFile) {
						realConfigFile = currentConfigFile
						err := v.ReadInConfig()
						if err != nil {
							log.Printf("error reading config file: %v\n", err)
						}
						if v.onConfigChange != nil {
							v.onConfigChange(event)
						}
					} else if filepath.Clean(event.Name) == configFile &&
						event.Op&fsnotify.Remove&fsnotify.Remove != 0 {
						eventsWG.Done()
						return
					}

				case err, ok := <-watcher.Errors:
					if ok { // 'Errors' channel is not closed
						log.Printf("watcher error: %v\n", err)
					}
					eventsWG.Done()
					return
				}
			}
		}()
		watcher.Add(configDir)
		initWG.Done()   // done initializing the watch in this go routine, so the parent routine can move on...
		eventsWG.Wait() // now, wait for event loop to end in this go-routine...
	}()
	initWG.Wait() // make sure that the go routine above fully ended before returning
}

// SetConfigFile explicitly defines the path, name and extension of the config file.
// Viper will use this and not check any of the config paths.
func SetConfigFile(in string) { v.SetConfigFile(in) }
func (v *Viper) SetConfigFile(in string) {
	if in != "" {
		v.lock.Lock()
		v.configFile = in
		v.lock.Unlock()
	}
}

// SetEnvPrefix defines a prefix that ENVIRONMENT variables will use.
// E.g. if your prefix is "spf", the env registry will look for env
// variables that start with "SPF_".
func SetEnvPrefix(in string) { v.SetEnvPrefix(in) }
func (v *Viper) SetEnvPrefix(in string) {
	if in != "" {
		v.lock.Lock()
		v.cache.Clear()
		v.envPrefix = in
		v.lock.Unlock()
	}
}

func (v *Viper) mergeWithEnvPrefix(in string) string {
	if v.envPrefix != "" {
		return strings.ToUpper(v.envPrefix + "_" + in)
	}

	return strings.ToUpper(in)
}

// AllowEmptyEnv tells Viper to consider set,
// but empty environment variables as valid values instead of falling back.
// For backward compatibility reasons this is false by default.
func AllowEmptyEnv(allowEmptyEnv bool) { v.AllowEmptyEnv(allowEmptyEnv) }
func (v *Viper) AllowEmptyEnv(allowEmptyEnv bool) {
	v.lock.Lock()
	v.cache.Clear()
	v.allowEmptyEnv = allowEmptyEnv
	v.lock.Unlock()
}

// TODO: should getEnv logic be moved into find(). Can generalize the use of
// rewriting keys many things, Ex: Get('someKey') -> some_key
// (camel case to snake case for JSON keys perhaps)

// getEnv is a wrapper around os.Getenv which replaces characters in the original
// key. This allows env vars which have different keys than the config object
// keys.
func (v *Viper) getEnv(key string) (string, bool) {
	if v.envKeyReplacer != nil {
		key = v.envKeyReplacer.Replace(key)
	}

	val, ok := os.LookupEnv(key)

	return val, ok && (v.allowEmptyEnv || val != "")
}

// ConfigFileUsed returns the file used to populate the config registry.
func ConfigFileUsed() string { return v.ConfigFileUsed() }
func (v *Viper) ConfigFileUsed() string {
	v.lock.RLock()
	defer v.lock.RUnlock()

	return v.configFile
}

// ConfigChangeAt returns the time of the last config change.
func ConfigChangeAt() time.Time { return v.ConfigChangeAt() }
func (v *Viper) ConfigChangeAt() time.Time {
	v.lock.RLock()
	defer v.lock.RUnlock()

	return v.configChangedAt
}

// AddConfigPath adds a path for Viper to search for the config file in.
// Can be called multiple times to define multiple search paths.
func AddConfigPath(in string) { v.AddConfigPath(in) }
func (v *Viper) AddConfigPath(in string) {
	if in != "" {
		absin := absPathify(in)
		jww.INFO.Println("adding", absin, "to paths to search")
		v.lock.Lock()
		if !stringInSlice(absin, v.configPaths) {
			v.cache.Clear()
			v.configPaths = append(v.configPaths, absin)
		}
		v.lock.Unlock()
	}
}

// AddRemoteProvider adds a remote configuration source.
// Remote Providers are searched in the order they are added.
// provider is a string value: "etcd", "consul" or "firestore" are currently supported.
// endpoint is the url.  etcd requires http://ip:port  consul requires ip:port
// path is the path in the k/v store to retrieve configuration
// To retrieve a config file called myapp.json from /configs/myapp.json
// you should set path to /configs and set config name (SetConfigName()) to
// "myapp"
func AddRemoteProvider(provider, endpoint, path string) error {
	return v.AddRemoteProvider(provider, endpoint, path)
}
func (v *Viper) AddRemoteProvider(provider, endpoint, path string) error {
	if !stringInSlice(provider, SupportedRemoteProviders) {
		return UnsupportedRemoteProviderError(provider)
	}
	if provider != "" && endpoint != "" {
		jww.INFO.Printf("adding %s:%s to remote provider list", provider, endpoint)
		rp := &defaultRemoteProvider{
			endpoint: endpoint,
			provider: provider,
			path:     path,
		}
		if !v.providerPathExists(rp) {
			v.lock.Lock()
			v.cache.Clear()
			v.remoteProviders = append(v.remoteProviders, rp)
			v.lock.Unlock()
		}
	}
	return nil
}

// AddSecureRemoteProvider adds a remote configuration source.
// Secure Remote Providers are searched in the order they are added.
// provider is a string value: "etcd", "consul" or "firestore" are currently supported.
// endpoint is the url.  etcd requires http://ip:port  consul requires ip:port
// secretkeyring is the filepath to your openpgp secret keyring.  e.g. /etc/secrets/myring.gpg
// path is the path in the k/v store to retrieve configuration
// To retrieve a config file called myapp.json from /configs/myapp.json
// you should set path to /configs and set config name (SetConfigName()) to
// "myapp"
// Secure Remote Providers are implemented with github.com/bketelsen/crypt
func AddSecureRemoteProvider(provider, endpoint, path, secretkeyring string) error {
	return v.AddSecureRemoteProvider(provider, endpoint, path, secretkeyring)
}

func (v *Viper) AddSecureRemoteProvider(provider, endpoint, path, secretkeyring string) error {
	if !stringInSlice(provider, SupportedRemoteProviders) {
		return UnsupportedRemoteProviderError(provider)
	}
	if provider != "" && endpoint != "" {
		jww.INFO.Printf("adding %s:%s to remote provider list", provider, endpoint)
		rp := &defaultRemoteProvider{
			endpoint:      endpoint,
			provider:      provider,
			path:          path,
			secretKeyring: secretkeyring,
		}
		if !v.providerPathExists(rp) {
			v.lock.Lock()
			v.cache.Clear()
			v.remoteProviders = append(v.remoteProviders, rp)
			v.lock.Unlock()
		}
	}
	return nil
}

func (v *Viper) providerPathExists(p *defaultRemoteProvider) bool {
	v.lock.RLock()
	defer v.lock.RUnlock()
	for _, y := range v.remoteProviders {
		if reflect.DeepEqual(y, p) {
			return true
		}
	}
	return false
}

// searchMap recursively searches for a value for path in source map.
// Returns nil if not found.
// Note: This assumes that the path entries and map keys are lower cased.
func (v *Viper) searchMap(source map[string]interface{}, path []string) interface{} {
	if len(path) == 0 {
		return source
	}

	next, ok := source[path[0]]
	if ok {
		// Fast path
		if len(path) == 1 {
			return next
		}

		// Nested case
		switch next.(type) {
		case map[interface{}]interface{}:
			return v.searchMap(cast.ToStringMap(next), path[1:])
		case map[string]interface{}:
			// Type assertion is safe here since it is only reached
			// if the type of `next` is the same as the type being asserted
			return v.searchMap(next.(map[string]interface{}), path[1:])
		default:
			// got a value but nested key expected, return "nil" for not found
			return nil
		}
	}
	return nil
}

// searchMapWithPathPrefixes recursively searches for a value for path in source map.
//
// While searchMap() considers each path element as a single map key, this
// function searches for, and prioritizes, merged path elements.
// e.g., if in the source, "foo" is defined with a sub-key "bar", and "foo.bar"
// is also defined, this latter value is returned for path ["foo", "bar"].
//
// This should be useful only at config level (other maps may not contain dots
// in their keys).
//
// Note: This assumes that the path entries and map keys are lower cased.
func (v *Viper) searchMapWithPathPrefixes(source map[string]interface{}, path []string) interface{} {
	if len(path) == 0 {
		return source
	}

	// search for path prefixes, starting from the longest one
	for i := len(path); i > 0; i-- {
		prefixKey := strings.ToLower(strings.Join(path[0:i], v.keyDelim))

		next, ok := source[prefixKey]
		if ok {
			// Fast path
			if i == len(path) {
				return next
			}

			// Nested case
			var val interface{}
			switch next.(type) {
			case map[interface{}]interface{}:
				val = v.searchMapWithPathPrefixes(cast.ToStringMap(next), path[i:])
			case map[string]interface{}:
				// Type assertion is safe here since it is only reached
				// if the type of `next` is the same as the type being asserted
				val = v.searchMapWithPathPrefixes(next.(map[string]interface{}), path[i:])
			default:
				// got a value but nested key expected, do nothing and look for next prefix
			}
			if val != nil {
				return val
			}
		}
	}

	// not found
	return nil
}

// isPathShadowedInDeepMap makes sure the given path is not shadowed somewhere
// on its path in the map.
// e.g., if "foo.bar" has a value in the given map, it “shadows”
//
//	"foo.bar.baz" in a lower-priority map
func (v *Viper) isPathShadowedInDeepMap(path []string, m map[string]interface{}) string {
	var parentVal interface{}
	for i := 1; i < len(path); i++ {
		parentVal = v.searchMap(m, path[0:i])
		if parentVal == nil {
			// not found, no need to add more path elements
			return ""
		}
		switch parentVal.(type) {
		case map[interface{}]interface{}:
			continue
		case map[string]interface{}:
			continue
		default:
			// parentVal is a regular value which shadows "path"
			return strings.Join(path[0:i], v.keyDelim)
		}
	}
	return ""
}

// isPathShadowedInFlatMap makes sure the given path is not shadowed somewhere
// in a sub-path of the map.
// e.g., if "foo.bar" has a value in the given map, it “shadows”
//
//	"foo.bar.baz" in a lower-priority map
func (v *Viper) isPathShadowedInFlatMap(path []string, mi interface{}) string {
	// unify input map
	var m map[string]interface{}
	switch mi.(type) {
	case map[string]string, map[string]FlagValue:
		m = cast.ToStringMap(mi)
	default:
		return ""
	}

	// scan paths
	var parentKey string
	for i := 1; i < len(path); i++ {
		parentKey = strings.Join(path[0:i], v.keyDelim)
		if _, ok := m[parentKey]; ok {
			return parentKey
		}
	}
	return ""
}

// isPathShadowedInAutoEnv makes sure the given path is not shadowed somewhere
// in the environment, when automatic env is on.
// e.g., if "foo.bar" has a value in the environment, it “shadows”
//
//	"foo.bar.baz" in a lower-priority map
func (v *Viper) isPathShadowedInAutoEnv(path []string) string {
	var parentKey string
	for i := 1; i < len(path); i++ {
		parentKey = strings.Join(path[0:i], v.keyDelim)
		if _, ok := v.getEnv(v.mergeWithEnvPrefix(parentKey)); ok {
			return parentKey
		}
	}
	return ""
}

// SetTypeByDefaultValue enables or disables the inference of a key value's
// type when the Get function is used based upon a key's default value as
// opposed to the value returned based on the normal fetch logic.
//
// For example, if a key has a default value of []string{} and the same key
// is set via an environment variable to "a b c", a call to the Get function
// would return a string slice for the key if the key's type is inferred by
// the default value and the Get function would return:
//
//	[]string {"a", "b", "c"}
//
// Otherwise the Get function would return:
//
//	"a b c"
func SetTypeByDefaultValue(enable bool) { v.SetTypeByDefaultValue(enable) }
func (v *Viper) SetTypeByDefaultValue(enable bool) {
	v.lock.Lock()
	v.cache.Clear()
	v.typeByDefValue = enable
	v.lock.Unlock()
}

// GetViper gets the global Viper instance.
func GetViper() *Viper {
	return v
}

// HasChanged returns true if a key has changed and the change has not been retrieved yet using `Get()` and all
// casters `GetString()`, `GetDuration()`, ...
//
// If the value has not been retrieved at all this will also return true.
func HasChanged(key string) bool { return v.HasChanged(key) }
func (v *Viper) HasChanged(key string) bool {
	lcaseKey := strings.ToLower(key)

	v.lock.RLock()
	value, ok := v.previousValues[lcaseKey]
	v.lock.RUnlock()
	if !ok {
		return IsSet(key)
	}

	// Avoid writing the change with v.find
	return !reflect.DeepEqual(value, v.find(lcaseKey, true))
}

// HasChangedSinceInit returns true if a key has changed and the change has not been retrieved yet using `Get()` and all
// casters `GetString()`, `GetDuration()`, ...
//
// If the value has not been retrieved before at all this will return false.
func HasChangedSinceInit(key string) bool { return v.HasChangedSinceInit(key) }
func (v *Viper) HasChangedSinceInit(key string) bool {
	lcaseKey := strings.ToLower(key)

	v.lock.RLock()
	value, ok := v.previousValues[lcaseKey]
	v.lock.RUnlock()
	if !ok {
		return false
	}

	// Avoid writing the change with v.find
	return !reflect.DeepEqual(value, v.find(lcaseKey, true))
}

func castAllSourcesE(t, val interface{}) (interface{}, error) {
	switch t.(type) {
	case time.Time:
		return cast.ToTimeE(val)
	case time.Duration:
		return cast.ToDurationE(val)
	}

	return val, nil
}

func castStringSourcesE(t interface{}, val string) (interface{}, error) {
	switch t.(type) {
	case bool:
		return cast.ToBoolE(val)
	case string:
		return val, nil
	case int32, int16, int8, int:
		return cast.ToIntE(val)
	case uint:
		return cast.ToUintE(val)
	case uint32:
		return cast.ToUint32E(val)
	case uint64:
		return cast.ToUint64E(val)
	case int64:
		return cast.ToInt64E(val)
	case float64, float32:
		return cast.ToFloat64E(val)
	case []string:
		s := strings.TrimPrefix(val, "[")
		s = strings.TrimSuffix(s, "]")
		res, err := readAsCSV(s)
		if err != nil {
			return []string{}, err
		}
		return res, nil
	case []int:
		s := strings.TrimPrefix(val, "[")
		s = strings.TrimSuffix(s, "]")
		res, err := readAsCSV(s)
		if err != nil {
			return []int{}, err
		}
		return cast.ToIntSliceE(res)
	}

	return castAllSourcesE(t, val)
}

func (v *Viper) getType(lcaseKey string) interface{} {
	v.lock.RLock()
	defer v.lock.RUnlock()

	path := strings.Split(lcaseKey, v.keyDelim)

	if typeVal := v.searchMap(v.types, path); typeVal != nil {
		return typeVal
	} else if v.typeByDefValue {
		return v.searchMap(v.defaults, path)
	}
	return nil
}

// Get can retrieve any value given the key to use.
// Get is case-insensitive for a key.
// Get has the behavior of returning the value associated with the first
// place from where it is set. Viper will check in the following order:
// override, flag, env, config file, key/value store, default
//
// Get returns an interface. For a specific value use one of the Get____ methods.
func GetE(key string) (interface{}, error) { return v.GetE(key) }
func (v *Viper) GetE(key string) (interface{}, error) {
	lcaseKey := strings.ToLower(key)

	val, err := v.cachedFindE(lcaseKey, true)
	if err != nil {
		return val, err
	}

	v.lock.Lock()
	v.previousValues[lcaseKey] = val
	v.lock.Unlock()

	return val, nil
}
func Get(key string) interface{} { return v.Get(key) }
func (v *Viper) Get(key string) interface{} {
	val, _ := v.GetE(key)
	return val
}

// Sub returns new Viper instance representing a sub tree of this instance.
// Sub is case-insensitive for a key.
func Sub(key string) *Viper { return v.Sub(key) }
func (v *Viper) Sub(key string) *Viper {
	subv := New()
	data := v.Get(key)
	if data == nil {
		return nil
	}

	if reflect.TypeOf(data).Kind() == reflect.Map {
		subv.config = cast.ToStringMap(data)
		return subv
	}
	return nil
}

// GetString returns the value associated with the key as a string.
func GetString(key string) string { return v.GetString(key) }
func (v *Viper) GetString(key string) string {
	return cast.ToString(v.Get(key))
}

// GetBool returns the value associated with the key as a boolean.
func GetBool(key string) bool { return v.GetBool(key) }
func (v *Viper) GetBool(key string) bool {
	return cast.ToBool(v.Get(key))
}

// GetInt returns the value associated with the key as an integer.
func GetInt(key string) int { return v.GetInt(key) }
func (v *Viper) GetInt(key string) int {
	return cast.ToInt(v.Get(key))
}

// GetInt32 returns the value associated with the key as an integer.
func GetInt32(key string) int32 { return v.GetInt32(key) }
func (v *Viper) GetInt32(key string) int32 {
	return cast.ToInt32(v.Get(key))
}

// GetInt64 returns the value associated with the key as an integer.
func GetInt64(key string) int64 { return v.GetInt64(key) }
func (v *Viper) GetInt64(key string) int64 {
	return cast.ToInt64(v.Get(key))
}

// GetUint returns the value associated with the key as an unsigned integer.
func GetUint(key string) uint { return v.GetUint(key) }
func (v *Viper) GetUint(key string) uint {
	return cast.ToUint(v.Get(key))
}

// GetUint32 returns the value associated with the key as an unsigned integer.
func GetUint32(key string) uint32 { return v.GetUint32(key) }
func (v *Viper) GetUint32(key string) uint32 {
	return cast.ToUint32(v.Get(key))
}

// GetUint64 returns the value associated with the key as an unsigned integer.
func GetUint64(key string) uint64 { return v.GetUint64(key) }
func (v *Viper) GetUint64(key string) uint64 {
	return cast.ToUint64(v.Get(key))
}

// GetFloat64 returns the value associated with the key as a float64.
func GetFloat64(key string) float64 { return v.GetFloat64(key) }
func (v *Viper) GetFloat64(key string) float64 {
	return cast.ToFloat64(v.Get(key))
}

// GetTime returns the value associated with the key as time.
func GetTime(key string) time.Time { return v.GetTime(key) }
func (v *Viper) GetTime(key string) time.Time {
	return cast.ToTime(v.Get(key))
}

// GetDuration returns the value associated with the key as a duration.
func GetDuration(key string) time.Duration { return v.GetDuration(key) }
func (v *Viper) GetDuration(key string) time.Duration {
	return cast.ToDuration(v.Get(key))
}

// GetIntSlice returns the value associated with the key as a slice of int values.
func GetIntSlice(key string) []int { return v.GetIntSlice(key) }
func (v *Viper) GetIntSlice(key string) []int {
	return cast.ToIntSlice(v.Get(key))
}

// GetStringSlice returns the value associated with the key as a slice of strings.
func GetStringSlice(key string) []string { return v.GetStringSlice(key) }
func (v *Viper) GetStringSlice(key string) []string {
	return cast.ToStringSlice(v.Get(key))
}

// GetStringMap returns the value associated with the key as a map of interfaces.
func GetStringMap(key string) map[string]interface{} { return v.GetStringMap(key) }
func (v *Viper) GetStringMap(key string) map[string]interface{} {
	return cast.ToStringMap(v.Get(key))
}

// GetStringMapString returns the value associated with the key as a map of strings.
func GetStringMapString(key string) map[string]string { return v.GetStringMapString(key) }
func (v *Viper) GetStringMapString(key string) map[string]string {
	return cast.ToStringMapString(v.Get(key))
}

// GetStringMapStringSlice returns the value associated with the key as a map to a slice of strings.
func GetStringMapStringSlice(key string) map[string][]string { return v.GetStringMapStringSlice(key) }
func (v *Viper) GetStringMapStringSlice(key string) map[string][]string {
	return cast.ToStringMapStringSlice(v.Get(key))
}

// GetSizeInBytes returns the size of the value associated with the given key
// in bytes.
func GetSizeInBytes(key string) uint { return v.GetSizeInBytes(key) }
func (v *Viper) GetSizeInBytes(key string) uint {
	sizeStr := cast.ToString(v.Get(key))
	return parseSizeInBytes(sizeStr)
}

// UnmarshalKey takes a single key and unmarshals it into a Struct.
func UnmarshalKey(key string, rawVal interface{}, opts ...DecoderConfigOption) error {
	return v.UnmarshalKey(key, rawVal, opts...)
}
func (v *Viper) UnmarshalKey(key string, rawVal interface{}, opts ...DecoderConfigOption) error {
	err := decode(v.Get(key), defaultDecoderConfig(rawVal, opts...))

	if err != nil {
		return err
	}

	return nil
}

// Unmarshal unmarshals the config into a Struct. Make sure that the tags
// on the fields of the structure are properly set.
func Unmarshal(rawVal interface{}, opts ...DecoderConfigOption) error {
	return v.Unmarshal(rawVal, opts...)
}
func (v *Viper) Unmarshal(rawVal interface{}, opts ...DecoderConfigOption) error {
	err := decode(v.AllSettings(), defaultDecoderConfig(rawVal, opts...))

	if err != nil {
		return err
	}

	return nil
}

// defaultDecoderConfig returns default mapsstructure.DecoderConfig with suppot
// of time.Duration values & string slices
func defaultDecoderConfig(output interface{}, opts ...DecoderConfigOption) *mapstructure.DecoderConfig {
	c := &mapstructure.DecoderConfig{
		Metadata:         nil,
		Result:           output,
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// A wrapper around mapstructure.Decode that mimics the WeakDecode functionality
func decode(input interface{}, config *mapstructure.DecoderConfig) error {
	decoder, err := mapstructure.NewDecoder(config)
	if err != nil {
		return err
	}
	return decoder.Decode(input)
}

// UnmarshalExact unmarshals the config into a Struct, erroring if a field is nonexistent
// in the destination struct.
func UnmarshalExact(rawVal interface{}, opts ...DecoderConfigOption) error {
	return v.UnmarshalExact(rawVal, opts...)
}
func (v *Viper) UnmarshalExact(rawVal interface{}, opts ...DecoderConfigOption) error {
	config := defaultDecoderConfig(rawVal, opts...)
	config.ErrorUnused = true

	err := decode(v.AllSettings(), config)

	if err != nil {
		return err
	}

	return nil
}

// BindPFlags binds a full flag set to the configuration, using each flag's long
// name as the config key.
func BindPFlags(flags *pflag.FlagSet) error { return v.BindPFlags(flags) }
func (v *Viper) BindPFlags(flags *pflag.FlagSet) error {
	return v.BindFlagValues(pflagValueSet{flags})
}

// BindPFlag binds a specific key to a pflag (as used by cobra).
// Example (where serverCmd is a Cobra instance):
//
//	serverCmd.Flags().Int("port", 1138, "Port to run Application server on")
//	Viper.BindPFlag("port", serverCmd.Flags().Lookup("port"))
func BindPFlag(key string, flag *pflag.Flag) error { return v.BindPFlag(key, flag) }
func (v *Viper) BindPFlag(key string, flag *pflag.Flag) error {
	return v.BindFlagValue(key, pflagValue{flag})
}

// BindFlagValues binds a full FlagValue set to the configuration, using each flag's long
// name as the config key.
func BindFlagValues(flags FlagValueSet) error { return v.BindFlagValues(flags) }
func (v *Viper) BindFlagValues(flags FlagValueSet) (err error) {
	flags.VisitAll(func(flag FlagValue) {
		if err = v.BindFlagValue(flag.Name(), flag); err != nil {
			return
		}
	})
	return nil
}

// BindFlagValue binds a specific key to a FlagValue.
func BindFlagValue(key string, flag FlagValue) error { return v.BindFlagValue(key, flag) }
func (v *Viper) BindFlagValue(key string, flag FlagValue) error {
	if flag == nil {
		return fmt.Errorf("flag for %q is nil", key)
	}
	lcaseKey := strings.ToLower(key)
	v.lock.Lock()
	v.cache.Clear()
	v.pflags[lcaseKey] = flag
	v.lock.Unlock()

	// this is to maintain backwards compatibility with pflag.ValueType()
	// one should use viper.SetType(...) instead
	v.lock.RLock()
	typ := v.searchMap(v.types, strings.Split(lcaseKey, v.keyDelim))
	v.lock.RUnlock()
	// only use the old api if no type was set using SetType(...)
	if typ == nil {
		switch flag.ValueType() {
		case "int", "int8", "int16", "int32", "int64":
			v.SetType(key, 0)
		case "bool":
			v.SetType(key, false)
		case "stringSlice":
			v.SetType(key, []string{})
		case "intSlice":
			v.SetType(key, []int{})
		default:
			// unknown type, don't set any
		}
	}
	return nil
}

// BindEnv binds a Viper key to a ENV variable.
// ENV variables are case sensitive.
// If only a key is provided, it will use the env key matching the key, uppercased.
// EnvPrefix will be used when set when env name is not provided.
func BindEnv(input ...string) error { return v.BindEnv(input...) }
func (v *Viper) BindEnv(input ...string) error {
	var key, envkey string
	if len(input) == 0 {
		return fmt.Errorf("missing key to bind to")
	}

	key = strings.ToLower(input[0])

	if len(input) == 1 {
		envkey = v.mergeWithEnvPrefix(key)
	} else {
		envkey = input[1]
	}

	v.lock.Lock()
	v.cache.Clear()
	v.env[key] = envkey
	v.lock.Unlock()

	return nil
}

// cachedFind uses Viper's cache to find a key's value and `v.find` if it is not available
// in the cache.
func (v *Viper) cachedFindE(lcaseKey string, flagDefault bool) (interface{}, error) {
	realKey := v.realKey(lcaseKey)

	v.lock.RLock()
	value, found := v.cache.Get(realKey)
	v.lock.RUnlock()
	if found {
		return value, nil
	}

	value, castErr := v.findE(realKey, flagDefault)
	if castErr != nil {
		return nil, castErr
	}

	v.lock.Lock()
	v.cache.Set(realKey, value, 0)
	v.lock.Unlock()

	return value, nil
}

func (v *Viper) cachedFind(lcaseKey string, flagDefault bool) interface{} {
	val, _ := v.cachedFindE(lcaseKey, flagDefault)
	return val
}

// Given a key, find the value.
//
// Viper will check to see if an alias exists first.
// Viper will then check in the following order:
// flag, env, config file, key/value store.
// Lastly, if no value was found and flagDefault is true, and if the key
// corresponds to a flag, the flag's default value is returned.
//
// Note: this assumes a lower-cased key given.
func (v *Viper) findE(lcaseKey string, flagDefault bool) (interface{}, error) {
	v.lock.RLock()
	var (
		val    interface{}
		exists bool
		path   = strings.Split(lcaseKey, v.keyDelim)
		nested = len(path) > 1
	)

	// compute the path through the nested maps to the nested value
	if nested && v.isPathShadowedInDeepMap(path, castMapStringToMapInterface(v.aliases)) != "" {
		v.lock.RUnlock()
		return nil, nil
	}
	v.lock.RUnlock()

	// if the requested key is an alias, then return the proper key
	lcaseKey = v.realKey(lcaseKey)
	typ := v.getType(lcaseKey)

	v.lock.RLock()
	defer v.lock.RUnlock()
	path = strings.Split(lcaseKey, v.keyDelim)
	nested = len(path) > 1

	// Set() override first
	val = v.searchMap(v.override, path)
	if val != nil {
		return castAllSourcesE(typ, val)
	}
	if nested && v.isPathShadowedInDeepMap(path, v.override) != "" {
		return nil, nil
	}

	// PFlag override next
	flag, exists := v.pflags[lcaseKey]
	if exists && flag.HasChanged() {
		return castStringSourcesE(typ, flag.ValueString())
	}
	if nested && v.isPathShadowedInFlatMap(path, v.pflags) != "" {
		return nil, nil
	}

	// Env override next
	if v.automaticEnvApplied {
		// even if it hasn't been registered, if automaticEnv is used,
		// check any Get request
		if val, ok := v.getEnv(v.mergeWithEnvPrefix(lcaseKey)); ok {
			return castStringSourcesE(typ, val)
		}
		if nested && v.isPathShadowedInAutoEnv(path) != "" {
			return nil, nil
		}
	}
	envkey, exists := v.env[lcaseKey]
	if exists {
		if val, ok := v.getEnv(envkey); ok {
			return castStringSourcesE(typ, val)
		}
	}
	if nested && v.isPathShadowedInFlatMap(path, v.env) != "" {
		return nil, nil
	}

	// Config file next
	val = v.searchMapWithPathPrefixes(v.config, path)
	if val != nil {
		return castAllSourcesE(typ, val)
	}
	if nested && v.isPathShadowedInDeepMap(path, v.config) != "" {
		return nil, nil
	}

	// K/V store next
	val = v.searchMap(v.kvstore, path)
	if val != nil {
		return castAllSourcesE(typ, val)
	}
	if nested && v.isPathShadowedInDeepMap(path, v.kvstore) != "" {
		return nil, nil
	}

	// Default next
	val = v.searchMap(v.defaults, path)
	if val != nil {
		return castAllSourcesE(typ, val)
	}
	if nested && v.isPathShadowedInDeepMap(path, v.defaults) != "" {
		return nil, nil
	}

	if flagDefault {
		// last chance: if no value is found and a flag does exist for the key,
		// get the flag's default value even if the flag's value has not been set.
		if flag, exists := v.pflags[lcaseKey]; exists {
			return castStringSourcesE(typ, flag.ValueString())
		}
		// last item, no need to check shadowing
	}

	return nil, nil
}
func (v *Viper) find(lcaseKey string, flagDefault bool) interface{} {
	val, _ := v.findE(lcaseKey, flagDefault)
	return val
}

func readAsCSV(val string) ([]string, error) {
	if val == "" {
		return []string{}, nil
	}
	stringReader := strings.NewReader(val)
	csvReader := csv.NewReader(stringReader)
	return csvReader.Read()
}

// IsSet checks to see if the key has been set in any of the data locations.
// IsSet is case-insensitive for a key.
func IsSet(key string) bool { return v.IsSet(key) }
func (v *Viper) IsSet(key string) bool {
	lcaseKey := strings.ToLower(key)
	val := v.cachedFind(lcaseKey, false)
	return val != nil
}

// AutomaticEnv has Viper check ENV variables for all.
// keys set in config, default & flags
func AutomaticEnv() { v.AutomaticEnv() }
func (v *Viper) AutomaticEnv() {
	v.lock.Lock()
	v.cache.Clear()
	v.automaticEnvApplied = true
	v.lock.Unlock()
}

// SetEnvKeyReplacer sets the strings.Replacer on the viper object
// Useful for mapping an environmental variable to a key that does
// not match it.
func SetEnvKeyReplacer(r *strings.Replacer) { v.SetEnvKeyReplacer(r) }
func (v *Viper) SetEnvKeyReplacer(r *strings.Replacer) {
	v.lock.Lock()
	v.cache.Clear()
	v.envKeyReplacer = r
	v.lock.Unlock()
}

// RegisterAlias creates an alias that provides another accessor for the same key.
// This enables one to change a name without breaking the application.
func RegisterAlias(alias string, key string) { v.RegisterAlias(alias, key) }
func (v *Viper) RegisterAlias(alias string, key string) {
	v.registerAlias(alias, strings.ToLower(key))
}

func (v *Viper) registerAlias(alias string, key string) {
	alias = strings.ToLower(alias)
	if alias != key && alias != v.realKey(key) {
		v.lock.RLock()
		_, exists := v.aliases[alias]
		v.lock.RUnlock()

		if !exists {
			v.lock.Lock()
			// if we alias something that exists in one of the maps to another
			// name, we'll never be able to get that value using the original
			// name, so move the config value to the new realkey.
			if val, ok := v.config[alias]; ok {
				delete(v.config, alias)
				v.config[key] = val
			}
			if val, ok := v.kvstore[alias]; ok {
				delete(v.kvstore, alias)
				v.kvstore[key] = val
			}
			if val, ok := v.defaults[alias]; ok {
				delete(v.defaults, alias)
				v.defaults[key] = val
			}
			if val, ok := v.override[alias]; ok {
				delete(v.override, alias)
				v.override[key] = val
			}
			v.aliases[alias] = key
			v.lock.Unlock()
		}
	} else {
		jww.WARN.Println("Creating circular reference alias", alias, key, v.realKey(key))
	}
}

func (v *Viper) realKey(key string) string {
	v.lock.RLock()
	newkey, exists := v.aliases[key]
	v.lock.RUnlock()

	if exists {
		jww.DEBUG.Println("Alias", key, "to", newkey)
		return v.realKey(newkey)
	}
	return key
}

// InConfig checks to see if the given key (or an alias) is in the config file.
func InConfig(key string) bool { return v.InConfig(key) }
func (v *Viper) InConfig(key string) bool {
	// if the requested key is an alias, then return the proper key
	key = v.realKey(key)
	v.lock.RLock()
	_, exists := v.config[key]
	v.lock.RUnlock()
	return exists
}

func (v *Viper) setInMap(key string, value interface{}, target map[string]interface{}) {
	key = v.realKey(strings.ToLower(key))
	value = toCaseInsensitiveValue(value)

	v.lock.RLock()
	path := strings.Split(key, v.keyDelim)
	lastKey := strings.ToLower(path[len(path)-1])
	deepestMap := deepSearch(target, path[0:len(path)-1])
	v.lock.RUnlock()

	v.lock.Lock()
	v.cache.Clear()
	// set innermost value
	deepestMap[lastKey] = value
	v.lock.Unlock()
}

// SetDefault sets the default value for this key.
// SetDefault is case-insensitive for a key.
// Default only used when no value is provided by the user via flag, config or ENV.
func SetDefault(key string, value interface{}) { v.SetDefault(key, value) }
func (v *Viper) SetDefault(key string, value interface{}) {
	v.setInMap(key, value, v.defaults)
}

// SetType sets the type for this key.
// This type is used for type conversions, e.g. a slice from an env var
// This function allows the default to be nil while still enabling those type conversions configured
// through SetTypeByDefaultValue
func SetType(key string, t interface{}) { v.SetType(key, t) }
func (v *Viper) SetType(key string, t interface{}) {
	v.setInMap(key, t, v.types)
}

// Set sets the value for the key in the override register.
// Set is case-insensitive for a key.
// Will be used instead of values obtained via
// flags, config file, ENV, default, or key/value store.
func Set(key string, value interface{}) { v.Set(key, value) }
func (v *Viper) Set(key string, value interface{}) {
	v.setInMap(key, value, v.override)
}

// ReadInConfig will discover and load the configuration file from disk
// and key/value stores, searching in one of the defined paths.
func ReadInConfig() error { return v.ReadInConfig() }
func (v *Viper) ReadInConfig() error {
	jww.INFO.Println("Attempting to read in config file")
	filename, err := v.getConfigFile()
	if err != nil {
		return err
	}

	if !stringInSlice(v.getConfigType(), SupportedExts) {
		return UnsupportedConfigError(v.getConfigType())
	}

	jww.DEBUG.Println("Reading file: ", filename)
	file, err := afero.ReadFile(v.fs, filename)
	if err != nil {
		return err
	}

	config := make(map[string]interface{})

	err = v.unmarshalReader(bytes.NewReader(file), config)
	if err != nil {
		return err
	}

	v.lock.Lock()
	v.cache.Clear()
	v.config = config
	v.configChangedAt = time.Now()
	v.lock.Unlock()

	return nil
}

// SetRawConfig overwrites the raw config.
func SetRawConfig(config map[string]interface{}) { v.SetRawConfig(config) }
func (v *Viper) SetRawConfig(config map[string]interface{}) {
	v.lock.Lock()
	defer v.lock.Unlock()
	insensitiviseMap(config)
	v.config = config
	v.configChangedAt = time.Now()
	v.cache.Clear()
}

// MergeInConfig merges a new configuration with an existing config.
func MergeInConfig() error { return v.MergeInConfig() }
func (v *Viper) MergeInConfig() error {
	jww.INFO.Println("Attempting to merge in config file")
	filename, err := v.getConfigFile()
	if err != nil {
		return err
	}

	if !stringInSlice(v.getConfigType(), SupportedExts) {
		return UnsupportedConfigError(v.getConfigType())
	}

	file, err := afero.ReadFile(v.fs, filename)
	if err != nil {
		return err
	}

	return v.MergeConfig(bytes.NewReader(file))
}

// ReadConfig will read a configuration file, setting existing keys to nil if the
// key does not exist in the file.
func ReadConfig(in io.Reader) error { return v.ReadConfig(in) }
func (v *Viper) ReadConfig(in io.Reader) error {
	v.lock.Lock()
	defer v.lock.Unlock()
	v.cache.Clear()
	v.config = make(map[string]interface{})
	v.configChangedAt = time.Now()
	return v.unmarshalReader(in, v.config)
}

// MergeConfig merges a new configuration with an existing config.
func MergeConfig(in io.Reader) error { return v.MergeConfig(in) }
func (v *Viper) MergeConfig(in io.Reader) error {
	cfg := make(map[string]interface{})
	if err := v.unmarshalReader(in, cfg); err != nil {
		return err
	}
	return v.MergeConfigMap(cfg)
}

// MergeConfigMap merges the configuration from the map given with an existing config.
// Note that the map given may be modified.
func MergeConfigMap(cfg map[string]interface{}) error { return v.MergeConfigMap(cfg) }
func (v *Viper) MergeConfigMap(cfg map[string]interface{}) error {
	v.lock.Lock()
	v.cache.Clear()
	if v.config == nil {
		v.config = make(map[string]interface{})
	}
	insensitiviseMap(cfg)
	mergeMaps(cfg, v.config, nil)
	v.configChangedAt = time.Now()
	v.lock.Unlock()
	return nil
}

// WriteConfig writes the current configuration to a file.
func WriteConfig() error { return v.WriteConfig() }
func (v *Viper) WriteConfig() error {
	filename, err := v.getConfigFile()
	if err != nil {
		return err
	}
	return v.writeConfig(filename, true)
}

// SafeWriteConfig writes current configuration to file only if the file does not exist.
func SafeWriteConfig() error { return v.SafeWriteConfig() }
func (v *Viper) SafeWriteConfig() error {
	if len(v.configPaths) < 1 {
		return errors.New("missing configuration for 'configPath'")
	}
	return v.SafeWriteConfigAs(filepath.Join(v.configPaths[0], v.configName+"."+v.configType))
}

// WriteConfigAs writes current configuration to a given filename.
func WriteConfigAs(filename string) error { return v.WriteConfigAs(filename) }
func (v *Viper) WriteConfigAs(filename string) error {
	return v.writeConfig(filename, true)
}

// SafeWriteConfigAs writes current configuration to a given filename if it does not exist.
func SafeWriteConfigAs(filename string) error { return v.SafeWriteConfigAs(filename) }
func (v *Viper) SafeWriteConfigAs(filename string) error {
	alreadyExists, err := afero.Exists(v.fs, filename)
	if alreadyExists && err == nil {
		return ConfigFileAlreadyExistsError(filename)
	}
	return v.writeConfig(filename, false)
}

func (v *Viper) writeConfig(filename string, force bool) error {
	jww.INFO.Println("Attempting to write configuration to file.")
	var configType string

	ext := filepath.Ext(filename)
	if ext != "" {
		configType = ext[1:]
	} else {
		configType = v.configType
	}
	if configType == "" {
		return fmt.Errorf("config type could not be determined for %s", filename)
	}

	if !stringInSlice(configType, SupportedExts) {
		return UnsupportedConfigError(configType)
	}
	if v.config == nil {
		v.config = make(map[string]interface{})
	}
	flags := os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	if !force {
		flags |= os.O_EXCL
	}
	f, err := v.fs.OpenFile(filename, flags, v.configPermissions)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := v.marshalWriter(f, configType); err != nil {
		return err
	}

	return f.Sync()
}

// Unmarshal a Reader into a map.
// Should probably be an unexported function.
func unmarshalReader(in io.Reader, c map[string]interface{}) error {
	return v.unmarshalReader(in, c)
}
func (v *Viper) unmarshalReader(in io.Reader, c map[string]interface{}) error {
	buf := new(bytes.Buffer)
	buf.ReadFrom(in)

	switch strings.ToLower(v.getConfigType()) {
	case "yaml", "yml":
		if err := yaml.Unmarshal(buf.Bytes(), &c); err != nil {
			return ConfigParseError{err}
		}

	case "json":
		if err := json.Unmarshal(buf.Bytes(), &c); err != nil {
			return ConfigParseError{err}
		}

	case "hcl":
		obj, err := hcl.Parse(buf.String())
		if err != nil {
			return ConfigParseError{err}
		}
		if err = hcl.DecodeObject(&c, obj); err != nil {
			return ConfigParseError{err}
		}

	case "toml":
		tree, err := toml.LoadReader(buf)
		if err != nil {
			return ConfigParseError{err}
		}
		tmap := tree.ToMap()
		for k, v := range tmap {
			c[k] = v
		}

	case "dotenv", "env":
		env, err := gotenv.StrictParse(buf)
		if err != nil {
			return ConfigParseError{err}
		}
		for k, v := range env {
			c[k] = v
		}

	case "properties", "props", "prop":
		v.properties = properties.NewProperties()
		var err error
		if v.properties, err = properties.Load(buf.Bytes(), properties.UTF8); err != nil {
			return ConfigParseError{err}
		}
		for _, key := range v.properties.Keys() {
			value, _ := v.properties.Get(key)
			// recursively build nested maps
			path := strings.Split(key, ".")
			lastKey := strings.ToLower(path[len(path)-1])
			deepestMap := deepSearch(c, path[0:len(path)-1])
			// set innermost value
			deepestMap[lastKey] = value
		}

	case "ini":
		cfg := ini.Empty()
		err := cfg.Append(buf.Bytes())
		if err != nil {
			return ConfigParseError{err}
		}
		sections := cfg.Sections()
		for i := 0; i < len(sections); i++ {
			section := sections[i]
			keys := section.Keys()
			for j := 0; j < len(keys); j++ {
				key := keys[j]
				value := cfg.Section(section.Name()).Key(key.Name()).String()
				c[section.Name()+"."+key.Name()] = value
			}
		}
	}

	insensitiviseMap(c)
	return nil
}

func (v *Viper) marshalWriter(f afero.File, configType string) error {
	c := v.AllSettings()
	switch configType {
	case "json":
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return ConfigMarshalError{err}
		}
		_, err = f.WriteString(string(b))
		if err != nil {
			return ConfigMarshalError{err}
		}

	case "hcl":
		b, err := json.Marshal(c)
		if err != nil {
			return ConfigMarshalError{err}
		}
		ast, err := hcl.Parse(string(b))
		if err != nil {
			return ConfigMarshalError{err}
		}
		err = printer.Fprint(f, ast.Node)
		if err != nil {
			return ConfigMarshalError{err}
		}

	case "prop", "props", "properties":
		if v.properties == nil {
			v.properties = properties.NewProperties()
		}
		p := v.properties
		for _, key := range v.AllKeys() {
			_, _, err := p.Set(key, v.GetString(key))
			if err != nil {
				return ConfigMarshalError{err}
			}
		}
		_, err := p.WriteComment(f, "#", properties.UTF8)
		if err != nil {
			return ConfigMarshalError{err}
		}

	case "dotenv", "env":
		lines := []string{}
		for _, key := range v.AllKeys() {
			envName := strings.ToUpper(strings.Replace(key, ".", "_", -1))
			val := v.Get(key)
			lines = append(lines, fmt.Sprintf("%v=%v", envName, val))
		}
		s := strings.Join(lines, "\n")
		if _, err := f.WriteString(s); err != nil {
			return ConfigMarshalError{err}
		}

	case "toml":
		t, err := toml.TreeFromMap(c)
		if err != nil {
			return ConfigMarshalError{err}
		}
		s := t.String()
		if _, err := f.WriteString(s); err != nil {
			return ConfigMarshalError{err}
		}

	case "yaml", "yml":
		b, err := yaml.Marshal(c)
		if err != nil {
			return ConfigMarshalError{err}
		}
		if _, err = f.WriteString(string(b)); err != nil {
			return ConfigMarshalError{err}
		}

	case "ini":
		keys := v.AllKeys()
		cfg := ini.Empty()
		ini.PrettyFormat = false
		for i := 0; i < len(keys); i++ {
			key := keys[i]
			lastSep := strings.LastIndex(key, ".")
			sectionName := key[:(lastSep)]
			keyName := key[(lastSep + 1):]
			if sectionName == "default" {
				sectionName = ""
			}
			cfg.Section(sectionName).Key(keyName).SetValue(Get(key).(string))
		}
		cfg.WriteTo(f)
	}
	return nil
}

func keyExists(k string, m map[string]interface{}) string {
	lk := strings.ToLower(k)
	for mk := range m {
		lmk := strings.ToLower(mk)
		if lmk == lk {
			return mk
		}
	}
	return ""
}

func castToMapStringInterface(
	src map[interface{}]interface{}) map[string]interface{} {
	tgt := map[string]interface{}{}
	for k, v := range src {
		tgt[fmt.Sprintf("%v", k)] = v
	}
	return tgt
}

func castMapStringToMapInterface(src map[string]string) map[string]interface{} {
	tgt := map[string]interface{}{}
	for k, v := range src {
		tgt[k] = v
	}
	return tgt
}

func castMapFlagToMapInterface(src map[string]FlagValue) map[string]interface{} {
	tgt := map[string]interface{}{}
	for k, v := range src {
		tgt[k] = v
	}
	return tgt
}

// mergeMaps merges two maps. The `itgt` parameter is for handling go-yaml's
// insistence on parsing nested structures as `map[interface{}]interface{}`
// instead of using a `string` as the key for nest structures beyond one level
// deep. Both map types are supported as there is a go-yaml fork that uses
// `map[string]interface{}` instead.
func mergeMaps(
	src, tgt map[string]interface{}, itgt map[interface{}]interface{}) {
	for sk, sv := range src {
		tk := keyExists(sk, tgt)
		if tk == "" {
			jww.TRACE.Printf("tk=\"\", tgt[%s]=%v", sk, sv)
			tgt[sk] = sv
			if itgt != nil {
				itgt[sk] = sv
			}
			continue
		}

		tv, ok := tgt[tk]
		if !ok {
			jww.TRACE.Printf("tgt[%s] != ok, tgt[%s]=%v", tk, sk, sv)
			tgt[sk] = sv
			if itgt != nil {
				itgt[sk] = sv
			}
			continue
		}

		svType := reflect.TypeOf(sv)
		tvType := reflect.TypeOf(tv)
		if svType != tvType {
			jww.ERROR.Printf(
				"svType != tvType; key=%s, st=%v, tt=%v, sv=%v, tv=%v",
				sk, svType, tvType, sv, tv)
			continue
		}

		jww.TRACE.Printf("processing key=%s, st=%v, tt=%v, sv=%v, tv=%v",
			sk, svType, tvType, sv, tv)

		switch ttv := tv.(type) {
		case map[interface{}]interface{}:
			jww.TRACE.Printf("merging maps (must convert)")
			tsv := sv.(map[interface{}]interface{})
			ssv := castToMapStringInterface(tsv)
			stv := castToMapStringInterface(ttv)
			mergeMaps(ssv, stv, ttv)
		case map[string]interface{}:
			jww.TRACE.Printf("merging maps")
			mergeMaps(sv.(map[string]interface{}), ttv, nil)
		default:
			jww.TRACE.Printf("setting value")
			tgt[tk] = sv
			if itgt != nil {
				itgt[tk] = sv
			}
		}
	}
}

// ReadRemoteConfig attempts to get configuration from a remote source
// and read it in the remote configuration registry.
func ReadRemoteConfig() error { return v.ReadRemoteConfig() }
func (v *Viper) ReadRemoteConfig() error {
	return v.getKeyValueConfig()
}

func WatchRemoteConfig() error { return v.WatchRemoteConfig() }
func (v *Viper) WatchRemoteConfig() error {
	return v.watchKeyValueConfig()
}

func (v *Viper) WatchRemoteConfigOnChannel() error {
	return v.watchKeyValueConfigOnChannel()
}

// Retrieve the first found remote configuration.
func (v *Viper) getKeyValueConfig() error {
	if RemoteConfig == nil {
		return RemoteConfigError("Enable the remote features by doing a blank import of the viper/remote package: '_ github.com/ory/viper/remote'")
	}

	v.lock.RLock()
	for _, rp := range v.remoteProviders {
		val, err := v.getRemoteConfig(rp)
		if err != nil {
			continue
		}
		v.lock.RUnlock()
		v.lock.Lock()
		v.kvstore = val
		v.lock.Unlock()
		return nil
	}
	v.lock.RUnlock()
	return RemoteConfigError("No Files Found")
}

func (v *Viper) getRemoteConfig(provider RemoteProvider) (map[string]interface{}, error) {
	reader, err := RemoteConfig.Get(provider)
	if err != nil {
		return nil, err
	}
	v.lock.RLock()
	err = v.unmarshalReader(reader, v.kvstore)
	v.lock.RUnlock()
	return v.kvstore, err
}

// Retrieve the first found remote configuration.
func (v *Viper) watchKeyValueConfigOnChannel() error {
	for _, rp := range v.remoteProviders {
		respc, _ := RemoteConfig.WatchChannel(rp)
		// Todo: Add quit channel
		go func(rc <-chan *RemoteResponse) {
			for {
				b := <-rc
				reader := bytes.NewReader(b.Value)
				v.unmarshalReader(reader, v.kvstore)
			}
		}(respc)
		return nil
	}
	return RemoteConfigError("No Files Found")
}

// Retrieve the first found remote configuration.
func (v *Viper) watchKeyValueConfig() error {
	for _, rp := range v.remoteProviders {
		val, err := v.watchRemoteConfig(rp)
		if err != nil {
			continue
		}
		v.kvstore = val
		return nil
	}
	return RemoteConfigError("No Files Found")
}

func (v *Viper) watchRemoteConfig(provider RemoteProvider) (map[string]interface{}, error) {
	reader, err := RemoteConfig.Watch(provider)
	if err != nil {
		return nil, err
	}
	err = v.unmarshalReader(reader, v.kvstore)
	return v.kvstore, err
}

// AllKeys returns all keys holding a value, regardless of where they are set.
// Nested keys are returned with a v.keyDelim separator
func AllKeys() []string { return v.AllKeys() }
func (v *Viper) AllKeys() []string {
	v.lock.RLock()
	defer v.lock.RUnlock()

	m := map[string]bool{}
	// add all paths, by order of descending priority to ensure correct shadowing
	m = v.flattenAndMergeMap(m, castMapStringToMapInterface(v.aliases), "")
	m = v.flattenAndMergeMap(m, v.override, "")
	m = v.mergeFlatMap(m, castMapFlagToMapInterface(v.pflags))
	m = v.mergeFlatMap(m, castMapStringToMapInterface(v.env))
	m = v.flattenAndMergeMap(m, v.config, "")
	m = v.flattenAndMergeMap(m, v.kvstore, "")
	m = v.flattenAndMergeMap(m, v.defaults, "")

	// convert set of paths to list
	a := make([]string, 0, len(m))
	for x := range m {
		a = append(a, x)
	}
	return a
}

// flattenAndMergeMap recursively flattens the given map into a map[string]bool
// of key paths (used as a set, easier to manipulate than a []string):
//   - each path is merged into a single key string, delimited with v.keyDelim
//   - if a path is shadowed by an earlier value in the initial shadow map,
//     it is skipped.
//
// The resulting set of paths is merged to the given shadow set at the same time.
func (v *Viper) flattenAndMergeMap(shadow map[string]bool, m map[string]interface{}, prefix string) map[string]bool {
	if shadow != nil && prefix != "" && shadow[prefix] {
		// prefix is shadowed => nothing more to flatten
		return shadow
	}
	if shadow == nil {
		shadow = make(map[string]bool)
	}

	var m2 map[string]interface{}
	if prefix != "" {
		prefix += v.keyDelim
	}
	for k, val := range m {
		fullKey := prefix + k
		switch val.(type) {
		case map[string]interface{}:
			m2 = val.(map[string]interface{})
		case map[interface{}]interface{}:
			m2 = cast.ToStringMap(val)
		default:
			// immediate value
			shadow[strings.ToLower(fullKey)] = true
			continue
		}
		// recursively merge to shadow map
		shadow = v.flattenAndMergeMap(shadow, m2, fullKey)
	}
	return shadow
}

// mergeFlatMap merges the given maps, excluding values of the second map
// shadowed by values from the first map.
func (v *Viper) mergeFlatMap(shadow map[string]bool, m map[string]interface{}) map[string]bool {
	// scan keys
outer:
	for k := range m {
		path := strings.Split(k, v.keyDelim)
		// scan intermediate paths
		var parentKey string
		for i := 1; i < len(path); i++ {
			parentKey = strings.Join(path[0:i], v.keyDelim)
			if shadow[parentKey] {
				// path is shadowed, continue
				continue outer
			}
		}
		// add key
		shadow[strings.ToLower(k)] = true
	}
	return shadow
}

// AllSettings merges all settings and returns them as a map[string]interface{}.
func AllSettingsE() (map[string]interface{}, error) { return v.AllSettingsE() }
func (v *Viper) AllSettingsE() (m map[string]interface{}, lastErr error) {
	m = map[string]interface{}{}
	// start from the list of keys, and construct the map one value at a time
	for _, k := range v.AllKeys() {
		value, err := v.GetE(k)
		if err != nil {
			// set last err but still continue as we still want to compute the result with the zero value for this specific key
			lastErr = err
		}
		if value == nil {
			// should not happen, since AllKeys() returns only keys holding a value,
			// check just in case anything changes
			continue
		}
		path := strings.Split(k, v.keyDelim)
		lastKey := strings.ToLower(path[len(path)-1])
		deepestMap := deepSearch(m, path[0:len(path)-1])
		// set innermost value
		deepestMap[lastKey] = value
	}
	m = toMapStringInterface(m).(map[string]interface{}) // This is safe because the input is map[string]interface{}
	return
}

func AllSettings() map[string]interface{} { return v.AllSettings() }
func (v *Viper) AllSettings() map[string]interface{} {
	m, _ := v.AllSettingsE()
	return m
}

// SetFs sets the filesystem to use to read configuration.
func SetFs(fs afero.Fs) { v.SetFs(fs) }
func (v *Viper) SetFs(fs afero.Fs) {
	v.lock.Lock()
	v.fs = fs
	v.lock.Unlock()
}

// SetConfigName sets name for the config file.
// Does not include extension.
func SetConfigName(in string) { v.SetConfigName(in) }
func (v *Viper) SetConfigName(in string) {
	if in != "" {
		v.lock.Lock()
		v.configName = in
		v.configFile = ""
		v.lock.Unlock()
	}
}

// SetConfigType sets the type of the configuration returned by the
// remote source, e.g. "json".
func SetConfigType(in string) { v.SetConfigType(in) }
func (v *Viper) SetConfigType(in string) {
	if in != "" {
		v.lock.Lock()
		v.configType = in
		v.lock.Unlock()
	}
}

// SetConfigPermissions sets the permissions for the config file.
func SetConfigPermissions(perm os.FileMode) { v.SetConfigPermissions(perm) }
func (v *Viper) SetConfigPermissions(perm os.FileMode) {
	v.lock.Lock()
	v.configPermissions = perm.Perm()
	v.lock.Unlock()
}

func (v *Viper) getConfigType() string {
	if v.configType != "" {
		return v.configType
	}

	cf, err := v.getConfigFile()
	if err != nil {
		return ""
	}

	ext := filepath.Ext(cf)

	if len(ext) > 1 {
		return ext[1:]
	}

	return ""
}

func (v *Viper) getConfigFile() (string, error) {
	if v.configFile == "" {
		cf, err := v.findConfigFile()
		if err != nil {
			return "", err
		}
		v.configFile = cf
	}
	return v.configFile, nil
}

func (v *Viper) searchInPath(in string) (filename string) {
	jww.DEBUG.Println("Searching for config in ", in)
	for _, ext := range SupportedExts {
		jww.DEBUG.Println("Checking for", filepath.Join(in, v.configName+"."+ext))
		if b, _ := exists(v.fs, filepath.Join(in, v.configName+"."+ext)); b {
			jww.DEBUG.Println("Found: ", filepath.Join(in, v.configName+"."+ext))
			return filepath.Join(in, v.configName+"."+ext)
		}
	}

	if v.configType != "" {
		if b, _ := exists(v.fs, filepath.Join(in, v.configName)); b {
			return filepath.Join(in, v.configName)
		}
	}

	return ""
}

// Search all configPaths for any config file.
// Returns the first path that exists (and is a config file).
func (v *Viper) findConfigFile() (string, error) {
	jww.INFO.Println("Searching for config in ", v.configPaths)

	for _, cp := range v.configPaths {
		file := v.searchInPath(cp)
		if file != "" {
			return file, nil
		}
	}
	return "", ConfigFileNotFoundError{v.configName, fmt.Sprintf("%s", v.configPaths)}
}

// Debug prints all configuration registries for debugging
// purposes.
func Debug() { v.Debug() }
func (v *Viper) Debug() {
	v.lock.RLock()
	defer v.lock.RUnlock()
	fmt.Printf("Aliases:\n%#v\n", v.aliases)
	fmt.Printf("Override:\n%#v\n", v.override)
	fmt.Printf("PFlags:\n%#v\n", v.pflags)
	fmt.Printf("Env:\n%#v\n", v.env)
	fmt.Printf("Key/Value Store:\n%#v\n", v.kvstore)
	fmt.Printf("Config:\n%#v\n", v.config)
	fmt.Printf("Defaults:\n%#v\n", v.defaults)
}
