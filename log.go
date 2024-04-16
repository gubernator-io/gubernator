package gubernator

import (
	"bufio"
	"context"
	"io"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// The FieldLogger interface generalizes the Entry and Logger types
type FieldLogger interface {
	WithField(key string, value interface{}) *logrus.Entry
	WithFields(fields logrus.Fields) *logrus.Entry
	WithError(err error) *logrus.Entry
	WithContext(ctx context.Context) *logrus.Entry
	WithTime(t time.Time) *logrus.Entry

	Tracef(format string, args ...interface{})
	Debugf(format string, args ...interface{})
	Infof(format string, args ...interface{})
	Printf(format string, args ...interface{})
	Warnf(format string, args ...interface{})
	Warningf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
	Fatalf(format string, args ...interface{})

	Log(level logrus.Level, args ...interface{})
	Debug(args ...interface{})
	Info(args ...interface{})
	Print(args ...interface{})
	Warn(args ...interface{})
	Warning(args ...interface{})
	Error(args ...interface{})
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
			log.Errorf("Error while reading from Writer: %s", err)
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
