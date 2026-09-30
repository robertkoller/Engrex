// Command engrex-cache runs the semantic caching proxy that sits between Engrex and
// Ollama, and the commands for inspecting and tuning it.
//
// Its own binary rather than a subcommand of engrex, for two reasons. It imports nothing
// that needs CGO, so it builds with a plain `go build` while engrex needs the system
// sqlite and a build tag. And it is a service in its own right: anything that speaks the
// Ollama API can point at it, not only this project.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/robertkoller/engrex/cache"
	"github.com/robertkoller/engrex/internal/config"
	"github.com/robertkoller/engrex/internal/embedder"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "engrex-cache",
		Short: "Semantic caching proxy for local LLM calls",
		Long: "A caching layer between Engrex and Ollama. It recognizes requests that are\n" +
			"semantically the same as ones already answered and serves the stored response,\n" +
			"instead of generating it again.",
		SilenceUsage: true,
	}

	root.AddCommand(serveCommand(), statsCommand(), enableCommand(), disableCommand(),
		invalidateCommand(), loadTestCommand(), tuneCommand(), calibrateCommand())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func serveCommand() *cobra.Command {
	options := cache.DefaultServerOptions()
	var syntheticLatency time.Duration
	var threshold float64

	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the caching proxy and its dashboard",
		Long: "Serves the Ollama API on the proxy port, and metrics plus a dashboard on the\n" +
			"other. Point Engrex at it with `engrex-cache enable`.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			options.SyntheticLatency = syntheticLatency
			if threshold > 0 {
				options.Policy.Thresholds[cache.ClassAnswer] = threshold
			}

			server, err := cache.NewServer(options)
			if err != nil {
				return err
			}

			fmt.Printf("proxy      http://%s      (point clients here instead of Ollama)\n", options.ProxyAddress)
			fmt.Printf("dashboard  http://%s\n", options.DashboardAddress)
			fmt.Printf("upstream   %s\n", options.Upstream)
			fmt.Printf("data       %s\n", options.DataDirectory)
			if syntheticLatency > 0 {
				fmt.Printf("\nSynthetic provider: generation returns canned text after %s.\n", syntheticLatency)
				fmt.Printf("For benchmarking the cache only — nothing here talks to a real model.\n")
			}
			fmt.Printf("\nEntries loaded: %d\n", server.Cache().Len())

			signals := make(chan os.Signal, 1)
			signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

			failed := make(chan error, 1)
			go func() { failed <- server.Start() }()

			select {
			case err := <-failed:
				// A bind failure has to surface. Started-but-listening-on-nothing is the
				// worst outcome available: every client just times out.
				if err != nil {
					return err
				}
			case <-signals:
				fmt.Println("\nshutting down")
			}
			return server.Stop()
		},
	}

	command.Flags().StringVar(&options.ProxyAddress, "proxy", options.ProxyAddress, "address for the Ollama-compatible proxy")
	command.Flags().StringVar(&options.DashboardAddress, "dashboard", options.DashboardAddress, "address for metrics and the dashboard")
	command.Flags().StringVar(&options.Upstream, "upstream", options.Upstream, "the real provider to forward misses to")
	command.Flags().StringVar(&options.DataDirectory, "data", options.DataDirectory, "where the journal and lookup log live")
	command.Flags().BoolVar(&options.ContextTolerant, "tolerant", false, "also match questions whose retrieved passages only mostly agree")
	command.Flags().IntVar(&options.Policy.MaxEntries, "max-entries", options.Policy.MaxEntries, "cap on stored entries before eviction")
	command.Flags().Float64Var(&threshold, "threshold", 0, "similarity an answer must reach to be served (default 0.70)")
	command.Flags().DurationVar(&syntheticLatency, "synthetic-upstream", 0, "benchmark mode: fake generation with this delay instead of calling a model")
	return command
}

func statsCommand() *cobra.Command {
	var dashboard string
	command := &cobra.Command{
		Use:   "stats",
		Short: "Show what the running cache has done",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			var snapshot cache.Snapshot
			if err := getJSON(dashboard+"/api/stats", &snapshot); err != nil {
				return err
			}
			cache.WriteStatsReport(os.Stdout, snapshot)
			return nil
		},
	}
	command.Flags().StringVar(&dashboard, "dashboard", "http://127.0.0.1:11436", "dashboard address")
	return command
}

// enableCommand points Engrex at the proxy by writing the config field, mirroring how
// `engrex mcp enable` works. Explicit rather than auto-detected: whether the cache is in
// the path should never be a guess, least of all while measuring it.
func enableCommand() *cobra.Command {
	var proxy string
	command := &cobra.Command{
		Use:   "enable",
		Short: "Point Engrex at the cache",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			configuration, err := config.Load()
			if err != nil {
				return err
			}
			configuration.OllamaURL = proxy
			if err := config.Save(configuration); err != nil {
				return err
			}
			fmt.Printf("Engrex will now send generation and embedding calls to %s\n", proxy)
			fmt.Println("Restart the daemon for it to take effect.")
			return nil
		},
	}
	command.Flags().StringVar(&proxy, "proxy", "http://127.0.0.1:11435", "proxy address to write into the config")
	return command
}

func disableCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Send Engrex straight to Ollama again",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			configuration, err := config.Load()
			if err != nil {
				return err
			}
			configuration.OllamaURL = ""
			if err := config.Save(configuration); err != nil {
				return err
			}
			fmt.Printf("Engrex will go straight to %s again.\n", config.DefaultOllamaURL)
			fmt.Println("Restart the daemon for it to take effect.")
			return nil
		},
	}
}

func invalidateCommand() *cobra.Command {
	var criteria cache.InvalidateCriteria
	var class string
	var proxy string

	command := &cobra.Command{
		Use:   "invalidate",
		Short: "Drop cached entries by model, class, or tag",
		Long: "Upgrade the generation model and every stored answer came from the old one:\n" +
			"drop them with --model. Edit a prompt builder and only that class is stale:\n" +
			"drop it with --class.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			criteria.Class = cache.Class(class)
			if !criteria.All && criteria.Model == "" && criteria.Class == "" && criteria.Tag == "" {
				return fmt.Errorf("nothing selected: pass --all, --model, --class or --tag")
			}

			body, err := json.Marshal(criteria)
			if err != nil {
				return err
			}
			response, err := http.Post(proxy+"/cache/invalidate", "application/json", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("the cache proxy is not reachable at %s: %w", proxy, err)
			}
			defer response.Body.Close() //nolint:errcheck

			var outcome struct {
				Invalidated int `json:"invalidated"`
			}
			if err := json.NewDecoder(response.Body).Decode(&outcome); err != nil {
				return err
			}
			fmt.Printf("Dropped %d entries.\n", outcome.Invalidated)
			return nil
		},
	}

	command.Flags().StringVar(&proxy, "proxy", "http://127.0.0.1:11435", "proxy address")
	command.Flags().BoolVar(&criteria.All, "all", false, "drop everything")
	command.Flags().StringVar(&criteria.Model, "model", "", "drop entries generated by this model")
	command.Flags().StringVar(&class, "class", "", "drop one class: answer, rerank, rewrite, verify, embed")
	command.Flags().StringVar(&criteria.Tag, "tag", "", "drop entries carrying this tag")
	return command
}

func loadTestCommand() *cobra.Command {
	options := cache.DefaultLoadTestOptions()
	var synthetic bool

	command := &cobra.Command{
		Use:   "loadtest",
		Short: "Drive a realistic query mix through the proxy and report what it saved",
		Long: "Replays a mix of unique, repeated and reworded questions built from the eval\n" +
			"golden set. Against a real model a run of this size takes hours, so start the\n" +
			"proxy with --synthetic-upstream and pass --synthetic here to say so in the report.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			options.Progress = os.Stdout
			fmt.Printf("Sending %d requests to %s (%.0f%% unique, %.0f%% reworded)...\n",
				options.Requests, options.ProxyURL, options.UniqueShare*100, options.ParaphraseShare*100)

			result, err := cache.RunLoadTest(options)
			if err != nil {
				return err
			}
			cache.WriteLoadTestReport(os.Stdout, result, synthetic)
			return nil
		},
	}

	command.Flags().StringVar(&options.ProxyURL, "proxy", options.ProxyURL, "proxy address")
	command.Flags().StringVar(&options.Model, "model", options.Model, "model name to send")
	command.Flags().IntVar(&options.Requests, "requests", options.Requests, "how many requests to send")
	command.Flags().IntVar(&options.Concurrency, "concurrency", options.Concurrency, "requests in flight at once")
	command.Flags().Float64Var(&options.UniqueShare, "unique", options.UniqueShare, "fraction of requests nobody has asked before")
	command.Flags().Float64Var(&options.ParaphraseShare, "paraphrase", options.ParaphraseShare, "fraction that reword an earlier question")
	command.Flags().Int64Var(&options.Seed, "seed", options.Seed, "seed, so two runs are comparable")
	command.Flags().BoolVar(&synthetic, "synthetic", false, "note in the report that the provider was synthetic")
	return command
}

func tuneCommand() *cobra.Command {
	var class string
	var dataDirectory string

	command := &cobra.Command{
		Use:   "tune",
		Short: "Replay the lookup log to see what a different threshold would have done",
		Long: "Every lookup records how close it came to a stored entry, so what a looser or\n" +
			"tighter threshold would have served is arithmetic over that log rather than\n" +
			"another run.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			path := filepath.Join(dataDirectory, "lookups.jsonl")
			records, err := cache.ReadLookupLog(path)
			if err != nil {
				return err
			}
			if len(records) == 0 {
				return fmt.Errorf("no lookups recorded yet at %s — run some queries through the proxy first", path)
			}

			selected := cache.Class(strings.TrimSpace(class))
			rows := cache.SweepThresholds(records, selected, cache.DefaultThresholds)

			current := cache.DefaultPolicy().Threshold(selected)
			fmt.Printf("Replaying %d lookups", len(records))
			if selected != "" {
				fmt.Printf(" for class %q", selected)
			}
			fmt.Println(".")
			cache.WriteThresholdReport(os.Stdout, rows, current)
			return nil
		},
	}

	command.Flags().StringVar(&class, "class", string(cache.ClassAnswer), "which class to tune: answer, rerank, rewrite, verify (empty for all)")
	command.Flags().StringVar(&dataDirectory, "data", cache.DefaultDataDirectory(), "where the lookup log lives")
	return command
}

func getJSON(url string, into any) error {
	response, err := http.Get(url) //nolint:noctx
	if err != nil {
		return fmt.Errorf("the cache proxy is not reachable at %s: %w", url, err)
	}
	defer response.Body.Close() //nolint:errcheck
	return json.NewDecoder(response.Body).Decode(into)
}

// calibrateCommand measures the two distributions every threshold depends on, using the
// workload's paraphrases as ground truth. Cheap — embeddings only, no generation — and
// the only honest way to pick a number, since the right one is a property of the
// embedding model rather than of the cache.
func calibrateCommand() *cobra.Command {
	var upstream string

	command := &cobra.Command{
		Use:   "calibrate",
		Short: "Measure how similar rewordings really are, and recommend a threshold",
		Long: "Embeds the workload's questions and their hand-written paraphrases, then\n" +
			"reports how well similarity separates 'the same question in other words' from\n" +
			"'a different question'. Needs Ollama for the embedding model; no generation.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			workload, err := cache.DefaultWorkload()
			if err != nil {
				return err
			}

			fmt.Printf("Embedding %d questions and their paraphrases via %s...\n",
				len(workload.Questions), upstream)
			pairs, err := cache.Calibrate(embedder.New(upstream), workload)
			if err != nil {
				return fmt.Errorf("embedding failed — is Ollama running? %w", err)
			}

			rows := cache.CalibrationSweep(pairs, cache.CalibrationThresholds)
			cache.WriteCalibrationReport(os.Stdout, pairs, rows,
				cache.DefaultPolicy().Threshold(cache.ClassAnswer))
			return nil
		},
	}

	command.Flags().StringVar(&upstream, "upstream", config.DefaultOllamaURL, "where the embedding model lives")
	return command
}
