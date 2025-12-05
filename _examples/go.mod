module github.com/lynx-go/lynx/examples

go 1.24.2

replace (
	github.com/lynx-go/lynx => ../
	github.com/lynx-go/lynx/contrib/kafka => ../contrib/kafka
	github.com/lynx-go/lynx/contrib/pubsub => ../contrib/pubsub
	github.com/lynx-go/lynx/contrib/schedule => ../contrib/schedule
	github.com/lynx-go/lynx/contrib/zap => ../contrib/zap
)

require (
	github.com/ThreeDotsLabs/watermill v1.5.1
	github.com/go-sql-driver/mysql v1.9.3
	github.com/google/uuid v1.6.0
	github.com/google/wire v0.7.0
	github.com/lynx-go/lynx v0.4.0
	github.com/lynx-go/lynx/contrib/kafka v0.0.0-00010101000000-000000000000
	github.com/lynx-go/lynx/contrib/pubsub v0.0.1
	github.com/lynx-go/lynx/contrib/schedule v0.0.0-20251205120803-d841a9e8d821
	github.com/lynx-go/lynx/contrib/zap v0.1.0
	github.com/lynx-go/x v0.0.5
	github.com/samber/lo v1.52.0
	github.com/spf13/pflag v1.0.10
	github.com/spf13/viper v1.21.0
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
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
	github.com/robfig/cron/v3 v3.0.1 // indirect
	github.com/sagikazarmark/locafero v0.12.0 // indirect
	github.com/samber/slog-common v0.18.1 // indirect
	github.com/samber/slog-zap/v2 v2.6.2 // indirect
	github.com/segmentio/kafka-go v0.4.49 // indirect
	github.com/sony/gobreaker v1.0.0 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.62.0 // indirect
	go.opentelemetry.io/otel v1.37.0 // indirect
	go.opentelemetry.io/otel/metric v1.37.0 // indirect
	go.opentelemetry.io/otel/trace v1.37.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.1 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	gocloud.dev v0.43.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.31.0 // indirect
)
