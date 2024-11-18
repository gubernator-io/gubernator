package gubernator

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"sync"
)

type FieldLogger interface {
	Handler() slog.Handler
	With(args ...any) *slog.Logger
	WithGroup(name string) *slog.Logger
	Enabled(ctx context.Context, level slog.Level) bool
	Log(ctx context.Context, level slog.Level, msg string, args ...any)
	LogAttrs(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr)
	Debug(msg string, args ...any)
	DebugContext(ctx context.Context, msg string, args ...any)
	Info(msg string, args ...any)
	InfoContext(ctx context.Context, msg string, args ...any)
	Warn(msg string, args ...any)
	WarnContext(ctx context.Context, msg string, args ...any)
	Error(msg string, args ...any)
	ErrorContext(ctx context.Context, msg string, args ...any)
}

func ErrAttr(err error) slog.Attr {
	return slog.Any("error", err)
}

type logAdaptor struct {
	writer *io.PipeWriter
	closer func()
}

func (l *logAdaptor) Write(p []byte) (n int, err error) {
	return l.writer.Write(p)
}

func (l *logAdaptor) Close() error {
	l.closer()
	return nil
}

func newLogAdaptor(log FieldLogger) *logAdaptor {
	reader, writer := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			log.Info(scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.LogAttrs(context.TODO(), slog.LevelError, "Error while reading from Writer",
				ErrAttr(err),
			)
		}
		_ = reader.Close()
		wg.Done()
	}()

	return &logAdaptor{
		writer: writer,
		closer: func() {
			_ = writer.Close()
			wg.Wait()
		},
	}
}
