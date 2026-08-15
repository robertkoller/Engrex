package main

import (
	"fmt"
	"os"

	"github.com/robertkoller/engrex/internal/hnsw"
	"github.com/spf13/cobra"
)

// benchCommand measures the hand-written HNSW index against brute-force search and
// reports where — if anywhere — the approximate path is actually worth using.
func benchCommand() *cobra.Command {
	var sizes []int
	var dimensions int
	var queries int
	var k int
	var m int
	var efConstruction int
	var efSearch int
	var efSweepSize int

	command := &cobra.Command{
		Use:   "bench-hnsw",
		Short: "Benchmark the HNSW index against exact search",
		Long: "Builds HNSW indexes over synthetic vectors at a range of corpus sizes and reports " +
			"build time, query latency against a brute-force baseline, recall@k, and memory. Ends " +
			"with the crossover size where approximate search starts beating exact — which on a " +
			"personal-scale corpus it usually does not.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			config := hnsw.DefaultBenchmarkConfig()
			if len(sizes) > 0 {
				config.Sizes = sizes
			}
			config.Dimensions = dimensions
			config.Queries = queries
			config.K = k
			config.Params = hnsw.Params{
				M:              m,
				EfConstruction: efConstruction,
				EfSearch:       efSearch,
				Seed:           42,
			}

			fmt.Printf("HNSW benchmark — M=%d efConstruction=%d efSearch=%d, %d queries, k=%d\n\n",
				m, efConstruction, efSearch, queries, k)

			rows, err := hnsw.Run(config, os.Stdout)
			if err != nil {
				return err
			}
			hnsw.WriteReport(rows, os.Stdout)

			if efSweepSize > 0 {
				fmt.Printf("\n\nefSearch trade-off at %d vectors:\n", efSweepSize)
				sweep, err := hnsw.RunEfSweep(config, efSweepSize, os.Stdout)
				if err != nil {
					return err
				}
				hnsw.WriteEfReport(sweep, os.Stdout)
			}
			return nil
		},
	}

	command.Flags().IntSliceVar(&sizes, "sizes", nil, "corpus sizes to sweep (default 1k,5k,10k,50k,100k)")
	command.Flags().IntVar(&dimensions, "dim", 768, "vector dimensions (768 matches nomic-embed-text)")
	command.Flags().IntVar(&queries, "queries", 50, "queries per corpus size")
	command.Flags().IntVar(&k, "k", 10, "neighbors to retrieve")
	command.Flags().IntVar(&m, "m", 16, "HNSW M — links per node per layer")
	command.Flags().IntVar(&efConstruction, "ef-construction", 200, "build-time candidate list size")
	command.Flags().IntVar(&efSearch, "ef-search", 64, "query-time candidate list size")
	command.Flags().IntVar(&efSweepSize, "ef-sweep-size", 10000, "corpus size for the efSearch sweep (0 to skip)")
	return command
}
