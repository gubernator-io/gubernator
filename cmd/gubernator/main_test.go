package main_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	cli "github.com/gubernator-io/gubernator/v3/cmd/gubernator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/proxy"

	cli "github.com/gubernator-io/gubernator/v2/cmd/gubernator"
)

var cliRunning = flag.Bool("test_cli_running", false, "True if running as a child process; used by TestCLI")

func TestCLI(t *testing.T) {
	if *cliRunning {
		if err := cli.Main(context.Background()); err != nil {
			//if !strings.Contains(err.Error(), "context deadline exceeded") {
			//	log.Print(err.Error())
			//	os.Exit(1)
			//}
			fmt.Print(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}

	tests := []struct {
		args     []string
		env      []string
		name     string
		contains string
	}{
		{
			name: "Should start with no config provided",
			env: []string{
				"GUBER_HTTP_ADDRESS=localhost:8080",
			},
			args:     []string{},
			contains: "HTTP Listening on",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := exec.Command(os.Args[0], append([]string{"--test.run=TestCLI", "--test_cli_running"}, tt.args...)...)
			var out bytes.Buffer
			c.Stdout = &out
			c.Stderr = &out
			c.Env = tt.env

			if err := c.Start(); err != nil {
				t.Fatal("failed to start child process: ", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()

			waitCh := make(chan struct{})
			go func() {
				_ = c.Wait()
				close(waitCh)
			}()

			err := waitForConnect(ctx, "localhost:1050", nil)
			assert.NoError(t, err)
			time.Sleep(time.Second * 1)

			err = c.Process.Signal(syscall.SIGTERM)
			require.NoError(t, err)

			<-waitCh
			assert.Contains(t, out.String(), tt.contains)
		})
	}
}

// waitForConnect waits until the passed address is accepting connections.
// It will continue to attempt a connection until context is canceled.
func waitForConnect(ctx context.Context, address string, cfg *tls.Config) error {
	if address == "" {
		return fmt.Errorf("waitForConnect() requires a valid address")
	}

	var errs []string
	for {
		var d proxy.ContextDialer
		if cfg != nil {
			d = &tls.Dialer{Config: cfg}
		} else {
			d = &net.Dialer{}
		}
		conn, err := d.DialContext(ctx, "tcp", address)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		errs = append(errs, err.Error())
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err().Error())
			return errors.New(strings.Join(errs, "\n"))
		}
		time.Sleep(time.Millisecond * 100)
		continue
	}
}
