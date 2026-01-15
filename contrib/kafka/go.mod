module github.com/lynx-go/lynx/contrib/kafka

go 1.24.2

toolchain go1.24.9

replace (
	github.com/lynx-go/lynx => ../../
	github.com/lynx-go/lynx/contrib/pubsub => ../pubsub
)

require (
	github.com/ThreeDotsLabs/watermill v1.5.1
	github.com/cenkalti/backoff/v5 v5.0.3
	github.com/lynx-go/lynx v0.3.7
	github.com/lynx-go/lynx/contrib/pubsub v0.0.1
	github.com/lynx-go/x v0.1.0
	github.com/segmentio/kafka-go v0.4.49
	github.com/spf13/cast v1.10.0
)

require (
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/lithammer/shortuuid/v3 v3.0.7 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/oklog/run v1.2.0 // indirect
	github.com/oklog/ulid v1.3.1 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sagikazarmark/locafero v0.12.0 // indirect
	github.com/samber/lo v1.52.0 // indirect
	github.com/sony/gobreaker v1.0.0 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/spf13/viper v1.21.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	gocloud.dev v0.44.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.30.0 // indirect
)
