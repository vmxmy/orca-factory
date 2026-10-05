package runx

import "context"

// Logger is the optional structured sink for external call diagnostics.
// Implementations must be safe for concurrent calls when shared by clients.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type loggerContextKey struct{ name string }
type loggerValue struct{ logger Logger }

// WithLogger attaches a logger to ctx. A nil logger disables an inherited sink.
func WithLogger(ctx context.Context, logger Logger) context.Context {
	return context.WithValue(ctx, loggerContextKey{name: "external-call-logger"}, loggerValue{logger})
}

// LoggerFromContext returns the injected logger, or nil when logging is disabled.
func LoggerFromContext(ctx context.Context) Logger {
	value, _ := ctx.Value(loggerContextKey{name: "external-call-logger"}).(loggerValue)
	return value.logger
}
