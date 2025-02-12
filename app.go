package lynx

import (
	"context"
	"emperror.dev/emperror"
	"errors"
	"fmt"
	"github.com/lynx-go/lynx/casing"
	"github.com/lynx-go/lynx/lifecycle"
	"github.com/lynx-go/lynx/options"
	"github.com/samber/lo/mutable"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type App interface {
	Run()
}

type option struct {
	name  string
	rtype reflect.Type
	path  []int
}

type Func func(ctx context.Context) error
type app[O any] struct {
	setup    func(hooks *lifecycle.Registry, o O)
	root     *cobra.Command
	name     string
	id       string
	version  string
	hooks    *lifecycle.Registry
	logger   *slog.Logger
	commands []Command
	options  []option
}

type Option[O any] func(*app[O])

func WithName[O any](name string) Option[O] {
	return func(a *app[O]) {
		a.name = name
	}
}

func WithID[O any](id string) Option[O] {
	return func(a *app[O]) { a.id = id }
}

func WithLogger[O any](logger *slog.Logger) Option[O] {
	return func(a *app[O]) { a.logger = logger }
}

func WithVersion[O any](v string) Option[O] {
	return func(a *app[O]) { a.version = v }
}

func WithCommands[O any](commands ...Command) Option[O] {
	return func(a *app[O]) { a.commands = append(a.commands, commands...) }
}

func WithSetup[O any](fn func(hooks *lifecycle.Registry, o O)) Option[O] {
	return func(a *app[O]) {
		a.setup = fn
	}
}

func New[O any](opts ...Option[O]) App {
	a := &app[O]{
		hooks: lifecycle.NewHooks(),
	}
	//basePath := filepath.Base(os.Args[0])
	for _, opt := range opts {
		opt(a)
	}
	a.root = &cobra.Command{
		Use:           a.name,
		Version:       a.version,
		SilenceErrors: true,
	}
	if a.logger == nil {
		a.logger = slog.Default().With("name", a.name, "id", a.id, "version", a.version)
		slog.SetDefault(a.logger)
	}
	//logger := a.logger

	var o O
	a.setupOptions(reflect.TypeOf(o), []int{})

	a.root.RunE = func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithCancel(cmd.Context())
		eg, sctx := errgroup.WithContext(ctx)
		stopHooks := []func(ctx context.Context){}
		for _, hk := range a.hooks.Range() {
			hk := hk
			stopHooks = append(stopHooks, hk.OnStop)
		}
		wg := sync.WaitGroup{}
		eg.Go(func() error {
			<-sctx.Done()
			stopCtx, cancel := context.WithTimeout(cmd.Context(), time.Second*10)
			defer cancel()
			mutable.Reverse(stopHooks)
			for _, hk := range stopHooks {
				hk(stopCtx)
			}
			return nil
		})
		for _, hk := range a.hooks.Range() {
			hk := hk
			wg.Add(1)
			eg.Go(func() error {
				wg.Done()
				return hk.OnStart(sctx)
			})
		}
		wg.Wait()

		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		eg.Go(func() error {
			select {
			case <-sctx.Done():
				return nil
			case <-quit:
				cancel()
				return nil
			}
		})

		if err := eg.Wait(); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}

	for _, c := range a.commands {
		cmd := &cobra.Command{
			Use:   c.Use(),
			Short: c.Desc(),
			Long:  c.Example(),
			RunE: func(cb *cobra.Command, args []string) error {
				eg, sctx := errgroup.WithContext(cb.Context())
				stopHooks := []func(ctx context.Context){}
				for _, hk := range c.Hooks() {
					hk := hk
					stopHooks = append(stopHooks, hk.OnStop)
				}
				defer func() {
					mutable.Reverse(stopHooks)
					for _, hk := range stopHooks {
						hk(sctx)
					}
				}()

				wg := sync.WaitGroup{}
				for _, hk := range c.Hooks() {
					hk := hk
					wg.Add(1)
					eg.Go(func() error {
						wg.Done()
						return hk.OnStart(sctx)
					})
				}
				wg.Wait()

				return c.Run(sctx, args)
			},
		}

		a.root.AddCommand(cmd)
	}

	return a
}

func (a *app[O]) Run() {

	var o O
	existing := a.root.PersistentPreRun
	a.root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		// Load config from args/env/files
		v := reflect.ValueOf(&o).Elem()
		flags := a.root.PersistentFlags()
		for _, opt := range a.options {
			f := v
			for _, i := range opt.path {
				f = f.Field(i)
			}
			var fv reflect.Value
			switch deref(opt.rtype).Kind() {
			case reflect.String:
				s, _ := flags.GetString(opt.name)
				fv = reflect.ValueOf(s)
			case reflect.Int, reflect.Int64:
				var i any
				if opt.rtype == durationType {
					i, _ = flags.GetDuration(opt.name)
				} else {
					i, _ = flags.GetInt64(opt.name)
				}
				fv = reflect.ValueOf(i).Convert(deref(opt.rtype))
			case reflect.Bool:
				b, _ := flags.GetBool(opt.name)
				fv = reflect.ValueOf(b)
			}

			if opt.rtype.Kind() == reflect.Ptr {
				ptr := reflect.New(fv.Type())
				ptr.Elem().Set(fv)
				fv = ptr
			}

			f.Set(fv)
		}
		a.setup(a.hooks, o)

		if existing != nil {
			existing(cmd, args)
		}

		// Set options in context, so custom commands can access it.
		cmd.SetContext(options.Context(cmd.Context(), o))
	}

	emperror.Panic(a.root.Execute())
}

func (a *app[O]) setupOptions(t reflect.Type, path []int) {
	var err error
	flags := a.root.PersistentFlags()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)

		if !field.IsExported() {
			// This isn't a public field, so we cannot use reflect.Value.Set with
			// it. This is usually a struct field with a lowercase name.
			fmt.Fprintln(os.Stderr, "warning: ignoring unexported options field", field.Name)
			continue
		}

		currentPath := append([]int{}, path...)
		currentPath = append(currentPath, i)

		fieldType := deref(field.Type)
		if field.Anonymous {
			// Embedded struct. This enables composition from e.g. company defaults.
			a.setupOptions(fieldType, currentPath)
			continue
		}

		name := field.Tag.Get("name")
		if name == "" {
			name = casing.Kebab(field.Name)
		}

		envName := "SERVICE_" + casing.Snake(name, strings.ToUpper)
		defaultValue := field.Tag.Get("default")
		if v, ok := os.LookupEnv(envName); ok {
			// Env vars will override the default value, which is used to document
			// what the value is if no options are passed.
			defaultValue = v
		}

		a.options = append(a.options, option{name, field.Type, currentPath})
		switch fieldType.Kind() {
		case reflect.String:
			flags.StringP(name, field.Tag.Get("short"), defaultValue, field.Tag.Get("doc"))
		case reflect.Int, reflect.Int64:
			var def int64
			if defaultValue != "" {
				if fieldType == durationType {
					var t time.Duration
					t, err = time.ParseDuration(defaultValue)
					def = int64(t)
				} else {
					def, err = strconv.ParseInt(defaultValue, 10, 64)
				}
				if err != nil {
					panic(err)
				}
			}
			if fieldType == durationType {
				flags.DurationP(name, field.Tag.Get("short"), time.Duration(def), field.Tag.Get("doc"))
			} else {
				flags.Int64P(name, field.Tag.Get("short"), def, field.Tag.Get("doc"))
			}
		case reflect.Bool:
			var def bool
			if defaultValue != "" {
				def, err = strconv.ParseBool(defaultValue)
				if err != nil {
					panic(err)
				}
			}
			flags.BoolP(name, field.Tag.Get("short"), def, field.Tag.Get("doc"))
		default:
			panic("Unsupported option type: " + field.Type.Kind().String())
		}
	}
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

var durationType = reflect.TypeOf((*time.Duration)(nil)).Elem()
