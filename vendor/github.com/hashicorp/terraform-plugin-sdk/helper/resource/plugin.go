package resource

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/hashicorp/terraform-plugin-sdk/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/helper/logging"
	grpcplugin "github.com/hashicorp/terraform-plugin-sdk/internal/helper/plugin"
	proto "github.com/hashicorp/terraform-plugin-sdk/tfplugin5"
	"github.com/hashicorp/terraform-plugin-sdk/plugin"
	"github.com/hashicorp/terraform-plugin-sdk/terraform"
	tftest "github.com/hashicorp/terraform-plugin-test/v2"
	testing "github.com/mitchellh/go-testing-interface"
)

func runProviderCommand(t testing.T, f func() error, wd *tftest.WorkingDir, factories map[string]terraform.ResourceProviderFactory) error {
	// don't point to this as a test failure location
	// point to whatever called it
	t.Helper()

	// for backwards compatibility, make this opt-in
	if os.Getenv("TF_ACCTEST_REATTACH") != "1" {
		log.Println("[DEBUG] TF_ACCTEST_REATTACH not set to 1, not using reattach-based testing")
		return f()
	}
	if acctest.TestHelper == nil {
		log.Println("[DEBUG] acctest.TestHelper is nil, assuming we're not using binary acceptance testing")
		return f()
	}
	log.Println("[DEBUG] TF_ACCTEST_REATTACH set to 1 and acctest.TestHelper is not nil, using reattach-based testing")

	// Run the providers in the same process as the test runner using the
	// reattach behavior in Terraform. This ensures we get test coverage
	// and enables the use of delve as a debugger.
	//
	// This behavior is only available in Terraform 0.12.26 and later.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// this is needed so Terraform doesn't default to expecting protocol 4;
	// we're skipping the handshake because Terraform didn't launch the
	// plugins.
	os.Setenv("PLUGIN_PROTOCOL_VERSIONS", "5")

	// Terraform 0.12.X and 0.13.X+ treat namespaceless providers
	// differently in terms of what namespace they default to. So we're
	// going to set both variations, as we don't know which version of
	// Terraform we're talking to. We're also going to allow overriding
	// the host or namespace using environment variables.
	var namespaces []string
	host := "registry.terraform.io"
	if v := os.Getenv("TF_ACC_PROVIDER_NAMESPACE"); v != "" {
		namespaces = append(namespaces, v)
	} else {
		namespaces = append(namespaces, "-", "hashicorp")
	}
	if v := os.Getenv("TF_ACC_PROVIDER_HOST"); v != "" {
		host = v
	}

	// Spin up gRPC servers for every provider factory, start a
	// WaitGroup to listen for all of the close channels.
	var wg sync.WaitGroup
	reattachInfo := map[string]tfexec.ReattachConfig{}
	for providerName, factory := range factories {
		// providerName may be returned as terraform-provider-foo, and
		// we need just foo. So let's fix that.
		providerName = strings.TrimPrefix(providerName, "terraform-provider-")

		provider, err := factory()
		if err != nil {
			return fmt.Errorf("unable to create provider %q from factory: %v", providerName, err)
		}

		// keep track of the running factory, so we can make sure it's
		// shut down.
		wg.Add(1)

		// configure the settings our plugin will be served with
		// the GRPCProviderFunc wraps a non-gRPC provider server
		// into a gRPC interface, and the logger just discards logs
		// from go-plugin.
		opts := &plugin.ServeOpts{
			GRPCProviderFunc: func() proto.ProviderServer {
				return grpcplugin.NewGRPCProviderServerShim(provider)
			},
			Logger: hclog.New(&hclog.LoggerOptions{
				Name:   "plugintest",
				Level:  hclog.Trace,
				Output: ioutil.Discard,
			}),
		}

		// let's actually start the provider server
		config, closeCh, err := plugin.DebugServe(ctx, opts)
		if err != nil {
			return fmt.Errorf("unable to serve provider %q: %v", providerName, err)
		}

		tfexecConfig := tfexec.ReattachConfig{
			Protocol: config.Protocol,
			Pid:      config.Pid,
			Test:     config.Test,
			Addr: tfexec.ReattachConfigAddr{
				Network: config.Addr.Network,
				String:  config.Addr.String,
			},
		}

		// plugin.DebugServe hijacks our log output location, so let's
		// reset it
		logging.SetTestOutput(t)

		// when the provider exits, remove one from the waitgroup
		// so we can track when everything is done
		go func(c <-chan struct{}) {
			<-c
			wg.Done()
		}(closeCh)

		// set our provider's reattachinfo in our map, once
		// for every namespace that different Terraform versions
		// may expect.
		for _, ns := range namespaces {
			reattachInfo[strings.TrimSuffix(host, "/")+"/"+
				strings.TrimSuffix(ns, "/")+"/"+
				providerName] = tfexecConfig
		}
	}

	// set the working directory reattach info that will tell Terraform how
	// to connect to our various running servers.
	wd.SetReattachInfo(reattachInfo)

	// ok, let's call whatever Terraform command the test was trying to
	// call, now that we know it'll attach back to those servers we just
	// started.
	err := f()
	if err != nil {
		log.Printf("[WARN] Got error running Terraform: %s", err)
	}

	// cancel the servers so they'll return. Otherwise, this closeCh won't
	// get closed, and we'll hang here.
	cancel()

	// wait for the servers to actually shut down; it may take a moment for
	// them to clean up, or whatever.
	// TODO: add a timeout here?
	// PC: do we need one? The test will time out automatically...
	wg.Wait()

	// once we've run the Terraform command, let's remove the reattach
	// information from the WorkingDir's environment. The WorkingDir will
	// persist until the next call, but the server in the reattach info
	// doesn't exist anymore at this point, so the reattach info is no
	// longer valid. In theory it should be overwritten in the next call,
	// but just to avoid any confusing bug reports, let's just unset the
	// environment variable altogether.
	wd.UnsetReattachInfo()

	// return any error returned from the orchestration code running
	// Terraform commands
	return err
}
