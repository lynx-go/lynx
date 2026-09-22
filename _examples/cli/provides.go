package main

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"sync"

	"github.com/google/wire"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/boot"
)

// ProviderSet 是 Wire 依赖图：boot.New 聚合 hooks 与服务，NewDeps/NewApp
// 产出双聚合出口（见 app.go）。
//
//go:generate wire
var ProviderSet = wire.NewSet(
	boot.New,
	NewConfig,
	NewStore,
	NewServices,
	NewServiceFactories,
	NewPreStarts,
	NewDrains,
	NewPreStops,
	NewPostStops,
	NewDeps,
	NewApp,
)

// AppConfig 与 config.yaml 的 store 段对应。
type AppConfig struct {
	Store struct {
		Path string `mapstructure:"path"`
	} `mapstructure:"store"`
}

func NewConfig(app lynx.App) (*AppConfig, error) {
	c := new(AppConfig)
	if err := app.Config().Unmarshal(c); err != nil {
		return nil, err
	}
	if c.Store.Path == "" {
		c.Store.Path = "store.json"
	}
	return c, nil
}

// Store 是文件支撑的 KV 存储：作为 lynx.Service 注册进 Bootstrap——
// 实现 Checker，命令的依赖等待（三级就绪解析）把它纳入门控；Stop 在
// 优雅关停时落盘。
type Store struct {
	path string

	mu   sync.Mutex
	data map[string]string
}

func NewStore(cfg *AppConfig) (*Store, error) {
	s := &Store{path: cfg.Store.Path, data: map[string]string{}}
	if bs, err := os.ReadFile(s.path); err == nil {
		if err := json.Unmarshal(bs, &s.data); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) Name() string { return "store" }

func (s *Store) Init(ctx lynx.AppContext) error {
	if ctx != nil {
		ctx.Logger("service", "store").InfoContext(ctx.Context(),
			"store loaded", "path", s.path, "keys", len(s.data))
	}
	return nil
}

// Start 保持服务 actor 形态：随框架生命周期运行，直到关停。
func (s *Store) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop 落盘：命令完成 → 框架优雅关停 → 此处把内存态写回文件。
func (s *Store) Stop(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bs, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, bs, 0o644)
}

// CheckHealth 使命令起跑前等待 Store 就绪（三级就绪解析的 Checker 层）。
func (s *Store) CheckHealth() error { return nil }

func (s *Store) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *Store) Get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func NewServices(store *Store) []lynx.Service {
	return []lynx.Service{store}
}

func NewServiceFactories() []lynx.ServiceFactory { return []lynx.ServiceFactory{} }

func NewPreStarts(app lynx.App) boot.PreStartHooks {
	return boot.PreStartHooks{
		func(ctx context.Context) error {
			app.Logger().InfoContext(ctx, "cli app starting")
			return nil
		},
	}
}

func NewDrains() boot.DrainHooks       { return boot.DrainHooks{} }
func NewPreStops() boot.PreStopHooks   { return boot.PreStopHooks{} }
func NewPostStops() boot.PostStopHooks { return boot.PostStopHooks{} }
