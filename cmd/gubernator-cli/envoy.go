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
	"os"
	"sort"
	"strings"

	guber "github.com/gubernator-io/gubernator/v2"
	"github.com/mailgun/holster/v4/setter"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/yaml.v3"
)

const envoyUsage = `usage: gubernator-cli envoy <apply|get|delete> [flags]

  apply -f policy.yaml    upsert every domain policy in the file
  get                     print every domain policy as YAML
  delete <domain>...      remove the named domains

Connection flags: -e <grpc address>, -config <env file> (or GUBER_GRPC_ADDRESS)
`

// Options carries the streams a command writes to.
type Options struct {
	Stdout io.Writer
	Stderr io.Writer
}

// policyFile is the YAML `envoy apply` reads and `envoy get` prints. Enum
// fields are names, not numbers, and behaviors is a list rather than bitflags.
type policyFile struct {
	Policies []policyYAML `yaml:"policies"`
}

type policyYAML struct {
	Domain         string   `yaml:"domain"`
	Algorithm      string   `yaml:"algorithm,omitempty"`
	Behaviors      []string `yaml:"behaviors,omitempty"`
	OnMissingLimit string   `yaml:"on_missing_limit,omitempty"`
	Version        int64    `yaml:"version,omitempty"`
	Origin         string   `yaml:"origin,omitempty"`
}

// Run executes the `envoy` subcommand tree; args start at "envoy".
func Run(ctx context.Context, args []string, opts Options) error {
	setter.SetDefault(&opts.Stdout, io.Writer(os.Stdout))
	setter.SetDefault(&opts.Stderr, io.Writer(os.Stderr))
	if len(args) < 2 || args[0] != "envoy" {
		return fmt.Errorf("%s", envoyUsage)
	}

	var configFile, grpcAddress, file string
	flags := flag.NewFlagSet("envoy "+args[1], flag.ContinueOnError)
	flags.SetOutput(opts.Stderr)
	flags.StringVar(&configFile, "config", "", "Environment config file")
	flags.StringVar(&grpcAddress, "e", "", "Gubernator GRPC endpoint address")
	if args[1] == "apply" {
		flags.StringVar(&file, "f", "", "Policy YAML file")
	}
	// Allow flags after positional args, e.g. `envoy delete checkout -e host:port`.
	// Everything after "--" is positional so domains that start with "-" stay reachable.
	var domains []string
	rest := args[2:]
	for {
		if err := flags.Parse(rest); err != nil {
			return err
		}
		if flags.NArg() == 0 {
			break
		}
		if i := len(rest) - flags.NArg() - 1; i >= 0 && rest[i] == "--" {
			domains = append(domains, flags.Args()...)
			break
		}
		domains = append(domains, flags.Arg(0))
		rest = flags.Args()[1:]
	}

	// Validate the input before touching the network so a bad file makes no RPC
	var policies []*guber.DomainPolicy
	switch args[1] {
	case "apply":
		if file == "" {
			return fmt.Errorf("envoy apply requires -f <policy.yaml>")
		}
		var err error
		if policies, err = readPolicyFile(file); err != nil {
			return err
		}
	case "delete":
		if len(domains) == 0 {
			return fmt.Errorf("envoy delete requires at least one domain")
		}
	case "get":
	default:
		return fmt.Errorf("unknown envoy subcommand %q\n%s", args[1], envoyUsage)
	}

	client, closer, err := dialPolicy(configFile, grpcAddress)
	if err != nil {
		return err
	}
	defer closer()

	switch args[1] {
	case "apply":
		resp, err := client.ApplyPolicies(ctx, &guber.ApplyPoliciesReq{Policies: policies})
		if err != nil {
			return fmt.Errorf("while applying policies: %w", err)
		}
		for _, p := range resp.Applied {
			_, _ = fmt.Fprintf(opts.Stdout, "applied %s version %d\n", p.Domain, p.Version)
		}
		warnUnreachable(opts.Stderr, resp.UnreachablePeers)
	case "delete":
		resp, err := client.DeletePolicies(ctx, &guber.DeletePoliciesReq{Domains: domains})
		if err != nil {
			return fmt.Errorf("while deleting policies: %w", err)
		}
		for _, domain := range domains {
			_, _ = fmt.Fprintf(opts.Stdout, "deleted %s\n", domain)
		}
		warnUnreachable(opts.Stderr, resp.UnreachablePeers)
	case "get":
		resp, err := client.ListPolicies(ctx, &guber.ListPoliciesReq{})
		if err != nil {
			return fmt.Errorf("while listing policies: %w", err)
		}
		out := policyFile{Policies: make([]policyYAML, 0, len(resp.Policies))}
		for _, p := range resp.Policies {
			out.Policies = append(out.Policies, toYAML(p))
		}
		sort.Slice(out.Policies, func(i, j int) bool { return out.Policies[i].Domain < out.Policies[j].Domain })
		b, err := yaml.Marshal(out)
		if err != nil {
			return err
		}
		_, err = opts.Stdout.Write(b)
		return err
	}
	return nil
}

func warnUnreachable(w io.Writer, peers []string) {
	if len(peers) > 0 {
		_, _ = fmt.Fprintf(w, "warning: peers did not acknowledge and will catch up on their next sync: %s\n",
			strings.Join(peers, ", "))
	}
}

// readPolicyFile parses and validates a policy file. Unknown keys anywhere in
// the file are an error so the reference RLS's config is never half-accepted.
func readPolicyFile(path string) ([]*guber.DomainPolicy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var file policyFile
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("while parsing %s: %w", path, err)
	}
	if len(file.Policies) == 0 {
		return nil, fmt.Errorf("%s contains no policies", path)
	}

	policies := make([]*guber.DomainPolicy, 0, len(file.Policies))
	for i, p := range file.Policies {
		policy, err := fromYAML(p)
		if err != nil {
			return nil, fmt.Errorf("%s: policies[%d]: %w", path, i, err)
		}
		policies = append(policies, policy)
	}
	return policies, nil
}

func fromYAML(p policyYAML) (*guber.DomainPolicy, error) {
	if p.Domain == "" {
		return nil, fmt.Errorf("domain is required")
	}
	out := &guber.DomainPolicy{Domain: p.Domain}
	if p.Algorithm != "" {
		v, ok := guber.Algorithm_value[p.Algorithm]
		if !ok {
			return nil, fmt.Errorf("unknown algorithm %q; choices are [%s]", p.Algorithm, enumChoices(guber.Algorithm_name))
		}
		out.Algorithm = guber.Algorithm(v)
	}
	for _, name := range p.Behaviors {
		v, ok := guber.Behavior_value[name]
		if !ok {
			return nil, fmt.Errorf("unknown behavior %q; choices are [%s]", name, enumChoices(guber.Behavior_name))
		}
		out.Behavior |= v
	}
	if p.OnMissingLimit != "" {
		v, ok := guber.MissingLimitAction_value[strings.ToUpper(p.OnMissingLimit)]
		if !ok {
			return nil, fmt.Errorf("unknown on_missing_limit %q; choices are [deny, allow, error]", p.OnMissingLimit)
		}
		out.OnMissingLimit = guber.MissingLimitAction(v)
	}
	return out, nil
}

func toYAML(p *guber.DomainPolicy) policyYAML {
	out := policyYAML{
		Domain:         p.Domain,
		Algorithm:      p.Algorithm.String(),
		OnMissingLimit: strings.ToLower(p.OnMissingLimit.String()),
		Version:        p.Version,
		Origin:         p.Origin,
	}
	for _, bit := range []guber.Behavior{
		guber.Behavior_NO_BATCHING, guber.Behavior_GLOBAL, guber.Behavior_DURATION_IS_GREGORIAN,
		guber.Behavior_RESET_REMAINING, guber.Behavior_MULTI_REGION, guber.Behavior_DRAIN_OVER_LIMIT,
	} {
		if guber.HasBehavior(guber.Behavior(p.Behavior), bit) {
			out.Behaviors = append(out.Behaviors, bit.String())
		}
	}
	return out
}

func enumChoices(names map[int32]string) string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// dialPolicy connects using the same config file, -e flag and TLS settings
// the load generator uses.
func dialPolicy(configFile, grpcAddress string) (guber.EnvoyPolicyV1Client, func(), error) {
	var reader io.Reader
	if configFile != "" {
		f, err := os.Open(configFile)
		if err != nil {
			return nil, nil, fmt.Errorf("while opening config file: %w", err)
		}
		defer f.Close()
		reader = f
	}
	conf, err := guber.SetupDaemonConfig(logrus.StandardLogger(), reader)
	if err != nil {
		return nil, nil, err
	}
	setter.SetOverride(&conf.GRPCListenAddress, grpcAddress)
	if configFile == "" && grpcAddress == "" && os.Getenv("GUBER_GRPC_ADDRESS") == "" {
		return nil, nil, fmt.Errorf("please provide a GRPC endpoint via -e or from a config " +
			"file via -config or set the env GUBER_GRPC_ADDRESS")
	}
	if err := guber.SetupTLS(conf.TLS); err != nil {
		return nil, nil, err
	}

	creds := insecure.NewCredentials()
	if conf.ClientTLS() != nil {
		creds = credentials.NewTLS(conf.ClientTLS())
	}
	// Propagate spans like DialV1Server does
	conn, err := grpc.NewClient(conf.GRPCListenAddress, grpc.WithTransportCredentials(creds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	if err != nil {
		return nil, nil, err
	}
	return guber.NewEnvoyPolicyV1Client(conn), func() { _ = conn.Close() }, nil
}
