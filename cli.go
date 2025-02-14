package lynx

import (
	"context"
	"emperror.dev/emperror"
	"fmt"
	"github.com/lynx-go/lynx/casing"
	"github.com/spf13/cobra"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type CLI interface {
	Run()
}

type option struct {
	name  string
	rtype reflect.Type
	path  []int
}

type Func func(ctx context.Context) error
type cli[O any] struct {
	root    *cobra.Command
	logger  *slog.Logger
	options []option
}

func parseOptions[O any](root *cobra.Command, options []option) O {
	var o O
	existing := root.PersistentPreRun
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		// Load config from args/env/files
		v := reflect.ValueOf(&o).Elem()
		flags := root.PersistentFlags()
		for _, opt := range options {
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
		if existing != nil {
			existing(cmd, args)
		}
		cmd.SetContext(context.WithValue(cmd.Context(), optionKey, &o))
	}

	return o
}

type optionCtx struct {
}

var optionKey = optionCtx{}

func bindCmd[O any](parentCmd *cobra.Command, subCmds []*Command[O]) {
	for _, subCmd := range subCmds {
		subCmd := subCmd
		cmd := &cobra.Command{
			Use:   subCmd.Name(),
			Short: subCmd.Usage(),
			Long:  subCmd.Usage(),
		}

		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			o := cmd.Context().Value(optionKey).(*O)
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			return subCmd.RunE(ctx, *o)
		}
		parentCmd.AddCommand(cmd)
		if len(subCmd.SubCommands()) > 0 {
			bindCmd(cmd, subCmd.SubCommands())
		}
	}
}

func NewCLI[O any](rootCmd *Command[O]) CLI {
	c := &cli[O]{}

	c.root = &cobra.Command{
		Use:           rootCmd.Name(),
		Short:         rootCmd.Usage(),
		Long:          rootCmd.Usage(),
		Aliases:       rootCmd.Aliases(),
		SilenceErrors: true,
	}

	var o O
	c.setupOptions(reflect.TypeOf(o), []int{})

	c.root.RunE = func(cmd *cobra.Command, args []string) error {
		o := cmd.Context().Value(optionKey).(*O)
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		return rootCmd.RunE(ctx, *o)
	}
	parentCmd := c.root
	bindCmd[O](parentCmd, rootCmd.SubCommands())

	return c
}

func (c *cli[O]) Run() {

	parseOptions[O](c.root, c.options)

	emperror.Panic(c.root.Execute())
}

func (c *cli[O]) setupOptions(t reflect.Type, path []int) {
	var err error
	flags := c.root.PersistentFlags()
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
			c.setupOptions(fieldType, currentPath)
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

		c.options = append(c.options, option{name, field.Type, currentPath})
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
