/*
Copyright 2018-2022 Mailgun Technologies Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/gubernator-io/gubernator/v3"
	"github.com/gubernator-io/gubernator/v3/tracing"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"k8s.io/klog/v2"
)

var log = slog.Default().With("category", "gubernator")
var Version = "dev-build"
var tracerCloser io.Closer

func main() {
	err := Main(context.Background())
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func Main(ctx context.Context) error {
	var configFile string

	log.LogAttrs(ctx, slog.LevelInfo, "Gubernator",
		slog.String("version", Version),
		slog.String("arch", runtime.GOARCH),
		slog.String("os", runtime.GOOS),
	)
	flags := flag.NewFlagSet("gubernator", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&configFile, "config", "", "environment config file")
	flags.BoolVar(&gubernator.DebugEnabled, "debug", false, "enable debug")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if !strings.Contains(err.Error(), "flag provided but not defined") {
			return fmt.Errorf("while parsing flags: %w", err)
		}
	}

	// in order to prevent logging to /tmp by k8s.io/client-go
	// and other kubernetes related dependencies which are using
	// klog (https://github.com/kubernetes/klog), we need to
	// initialize klog in the way it prints to stderr only.
	klog.InitFlags(nil)
	_ = flag.Set("logtostderr", "true")

	res, err := tracing.NewResource("gubernator", Version, resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceInstanceID(gubernator.GetInstanceID()),
	))
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "during tracing.NewResource()",
			gubernator.ErrAttr(err),
		)
		return err
	}
	defer func() {
		if tracerCloser != nil {
			_ = tracerCloser.Close()
		}
	}()

	// Initialize tracing.
	shutdown, err := tracing.InitTracing(ctx, log,
		"github.com/gubernator-io/gubernator/v3",
		sdktrace.WithResource(res))
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "during tracing.InitTracing()",
			gubernator.ErrAttr(err),
		)
		return err
	}

	var configFileReader io.Reader
	// Read our config from the environment or optional environment config file
	if configFile != "" {
		configFileReader, err = os.Open(configFile)
		if err != nil {
			log.LogAttrs(ctx, slog.LevelError, "while opening config file",
				gubernator.ErrAttr(err),
			)
			return err
		}
	}

	conf, err := gubernator.SetupDaemonConfig(slog.Default(), configFileReader)
	if err != nil {
		return fmt.Errorf("while collecting daemon config: %w", err)
	}

	// Start the daemon
	daemon, err := gubernator.SpawnDaemon(ctx, conf)
	if err != nil {
		return fmt.Errorf("while spawning daemon: %w", err)
	}

	// Wait here for signals to clean up our mess
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-c:
		log.Info("caught signal; shutting down")
		_ = daemon.Close(context.Background())
		_ = shutdown(ctx)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
