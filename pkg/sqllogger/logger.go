package sqllogger

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	"github.com/lynx-go/x/log"
	sqldblogger "github.com/qiulin/sqldb-logger"
)

func WrapDB(db *sql.DB, source string) *sql.DB {
	return sqldblogger.OpenDriver(source, db.Driver(), &ctxLogger{},
		sqldblogger.WithTimeFieldname("execute_time"),
		sqldblogger.WithDurationUnit(sqldblogger.DurationMillisecond),
		sqldblogger.WithTimeFormat(sqldblogger.TimeFormatRFC3339Nano),
		sqldblogger.WithPreparerLevel(sqldblogger.LevelTrace),
		sqldblogger.WithCloserLevel(sqldblogger.LevelTrace),
		sqldblogger.WithUIDGenerator(&idGen{}),
	)
}

type idGen struct {
}

func (i *idGen) UniqueID() string {
	return uuid.NewString()
}

var _ sqldblogger.UIDGenerator = new(idGen)

type ctxLogger struct {
}

func (logger *ctxLogger) Log(ctx context.Context, level sqldblogger.Level, msg string, data map[string]interface{}) {
	vals := []any{}
	for k, v := range data {
		vals = append(vals, k, v)
	}
	switch level {
	case sqldblogger.LevelTrace:
	case sqldblogger.LevelDebug:
		log.DebugContext(ctx, msg, vals...)
	case sqldblogger.LevelInfo:
		log.InfoContext(ctx, msg, vals...)
	case sqldblogger.LevelError:
		log.ErrorContext(ctx, msg, nil, vals...)
	}
}

var _ sqldblogger.Logger = new(ctxLogger)
