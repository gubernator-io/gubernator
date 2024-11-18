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
	"log/slog"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/mailgun/holster/v4/clock"
	"github.com/mailgun/holster/v4/errors"
	"github.com/mailgun/holster/v4/setter"
	"github.com/mailgun/holster/v4/syncutil"
	"github.com/mailgun/holster/v4/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"

	guber "github.com/gubernator-io/gubernator/v3"
)

var (
	log                     *slog.Logger
	configFile, httpAddress string
	concurrency             uint64
	timeout                 time.Duration
	checksPerRequest        uint64
	reqRate                 float64
	quiet                   bool
)

func main() {
	flag.StringVar(&configFile, "config", "", "Environment config file")
	flag.StringVar(&httpAddress, "e", "", "Gubernator HTTP endpoint address")
	flag.Uint64Var(&concurrency, "concurrency", 1, "Concurrent threads (default 1)")
	flag.DurationVar(&timeout, "timeout", 100*time.Millisecond, "Request timeout (default 100ms)")
	flag.Uint64Var(&checksPerRequest, "checks", 1, "Rate checks per request (default 1)")
	flag.Float64Var(&reqRate, "rate", 0, "Request rate overall, 0 = no rate limit")
	flag.BoolVar(&quiet, "q", false, "Quiet logging")
	flag.Parse()

	log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: func() slog.Level {
			if quiet {
				return slog.LevelError
			}
			return slog.LevelInfo
		}(),
	}))

	// Initialize tracing.
	res, err := tracing.NewResource("gubernator-cli", "")
	if err != nil {
		log.LogAttrs(context.TODO(), slog.LevelError, "Error in tracing.NewResource",
			guber.ErrAttr(err),
		)
		return
	}
	ctx := context.Background()
	err = tracing.InitTracing(ctx,
		"github.com/gubernator-io/gubernator/v3/cmd/gubernator-cli",
		tracing.WithResource(res),
	)
	if err != nil {
		log.LogAttrs(context.TODO(), slog.LevelWarn, "Error in tracing.InitTracing",
			guber.ErrAttr(err),
		)
	}

	// Print startup message.
	startCtx := tracing.StartScope(ctx)
	argsMsg := fmt.Sprintf("Command line: %s", strings.Join(os.Args[1:], " "))
	log.Info(argsMsg)
	tracing.EndScope(startCtx, nil)

	var client guber.Client
	err = tracing.CallScope(ctx, func(ctx context.Context) error {
		// Print startup message.
		cmdLine := strings.Join(os.Args[1:], " ")
		log.LogAttrs(ctx, slog.LevelInfo, "Command Line",
			slog.String("cmdLine", cmdLine),
		)

		configFileReader, err := os.Open(configFile)
		if err != nil {
			return fmt.Errorf("while opening config file: %s", err)
		}
		conf, err := guber.SetupDaemonConfig(log, configFileReader)
		if err != nil {
			return err
		}
		setter.SetOverride(&conf.HTTPListenAddress, httpAddress)

		if configFile == "" && httpAddress == "" && os.Getenv("GUBER_GRPC_ADDRESS") == "" {
			return errors.New("please provide a GRPC endpoint via -e or from a config " +
				"file via -config or set the env GUBER_GRPC_ADDRESS")
		}

		err = guber.SetupTLS(conf.TLS)
		if err != nil {
			return err
		}

		log.LogAttrs(context.TODO(), slog.LevelInfo, "Connecting to",
			slog.String("address", conf.HTTPListenAddress),
		)
		client, err = guber.NewClient(guber.WithDaemonConfig(conf, conf.HTTPListenAddress))
		return err
	})

	if err != nil {
		log.LogAttrs(context.TODO(), slog.LevelError, err.Error(),
			guber.ErrAttr(err),
		)
		return
	}

	// Generate a selection of rate limits with random limits.
	var rateLimits []*guber.RateLimitRequest

	for i := 0; i < 2000; i++ {
		rateLimits = append(rateLimits, &guber.RateLimitRequest{
			Name:      fmt.Sprintf("gubernator-cli-%d", i),
			UniqueKey: guber.RandomString(10),
			Hits:      1,
			Limit:     int64(randInt(1, 1000)),
			Duration:  int64(randInt(int(clock.Millisecond*500), int(clock.Second*6))),
			Behavior:  guber.Behavior_BATCHING,
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
		})
	}

	fan := syncutil.NewFanOut(int(concurrency))
	var limiter *rate.Limiter
	if reqRate > 0 {
		l := rate.Limit(reqRate)
		log.LogAttrs(context.TODO(), slog.LevelInfo, "rate",
			slog.Float64("rate", reqRate),
		)
		limiter = rate.NewLimiter(l, 1)
	}

	// Replay requests in endless loop.
	for {
		for i := int(0); i < len(rateLimits); i += int(checksPerRequest) {
			req := &guber.CheckRateLimitsRequest{
				Requests: rateLimits[i:min(i+int(checksPerRequest), len(rateLimits))],
			}

			fan.Run(func(obj interface{}) error {
				req := obj.(*guber.CheckRateLimitsRequest)

				if reqRate > 0 {
					_ = limiter.Wait(ctx)
				}

				sendRequest(ctx, client, req)

				return nil
			}, req)
		}
	}
}

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

func randInt(min, max int) int {
	return rand.Intn(max-min) + min
}

func sendRequest(ctx context.Context, client guber.Client, req *guber.CheckRateLimitsRequest) {
	ctx = tracing.StartScope(ctx)
	defer tracing.EndScope(ctx, nil)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Now hit our cluster with the rate limits
	var resp guber.CheckRateLimitsResponse
	if err := client.CheckRateLimits(ctx, req, &resp); err != nil {
		log.LogAttrs(ctx, slog.LevelError, "Error in client.GetRateLimits",
			guber.ErrAttr(err),
		)
		return
	}

	// Sanity checks.
	if resp.Responses == nil {
		log.LogAttrs(ctx, slog.LevelError, "Responses array is unexpectedly nil")
		return
	}

	// Check for over limit response.
	overLimit := false

	for itemNum, resp := range resp.Responses {
		if resp.Status == guber.Status_OVER_LIMIT {
			overLimit = true
			log.LogAttrs(ctx, slog.LevelInfo, "Overlimit!",
				slog.String("name", req.Requests[itemNum].Name),
			)
		}
	}

	if overLimit {
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(
			attribute.Bool("overlimit", true),
		)

		if !quiet {
			dumpResp := spew.Sdump(&resp)
			log.LogAttrs(ctx, slog.LevelInfo, "Dump",
				slog.String("value", dumpResp),
			)
		}
	}
}
