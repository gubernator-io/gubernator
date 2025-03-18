/*
Copyright 2018-2023 Mailgun Technologies Inc

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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/mailgun/holster/v4/retry"

	guber "github.com/gubernator-io/gubernator/v2"
)

func main() {
	ctx := context.Background()

	url := os.Getenv("GUBER_HTTP_ADDRESS")
	if url == "" {
		url = "localhost:1050"
	}

	attemptsStr := os.Getenv("GUBER_HTTP_RETRY_COUNT")
	err := check(ctx, url, attemptsStr)
	switch {
	case errors.Is(err, errNotHealthy):
		fmt.Println(err)
		os.Exit(2)
	case err != nil:
		fmt.Println(err)
		os.Exit(1)
	default:
		fmt.Println("is healthy")
	}
}

var errNotHealthy = errors.New("not healthy")

func check(ctx context.Context, url string, attemptsStr string) (err error) {
	attempts, err := parseAttempts(attemptsStr)
	if err != nil {
		return err
	}

	return retry.Until(ctx, retry.Attempts(attempts, 500*time.Millisecond), func(ctx context.Context, i int) error {
		reqURL := fmt.Sprintf("http://%s/v1/HealthCheck", url)
		fmt.Printf("checking %q: attempt=%d\n", reqURL, i)

		resp, err := http.Get(reqURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		var hc guber.HealthCheckResp
		err = json.Unmarshal(body, &hc)
		if err != nil {
			return err
		}

		if hc.Status != "healthy" {
			return fmt.Errorf("%w: status=%q message=%q peer_count=%d advertise_address=%q", errNotHealthy,
				hc.Status,
				hc.Message,
				hc.PeerCount,
				hc.AdvertiseAddress,
			)
		}

		return nil
	})
}

func parseAttempts(attemptsStr string) (int, error) {
	if attemptsStr == "" {
		return 1, nil
	}

	return strconv.Atoi(attemptsStr)
}
