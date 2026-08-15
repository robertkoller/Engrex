package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertkoller/engrex/internal/eval"
	ragpkg "github.com/robertkoller/engrex/internal/rag"
	"github.com/spf13/cobra"
)

// defaultGoldenSet and defaultBaseline live in the repo rather than ~/.engrex — they
// are project artifacts that belong in version control, since the whole point of a
// baseline is comparing against what was committed before a change.
const defaultGoldenSet = "eval/golden.json"
const defaultBaseline = "eval/baseline.json"

// evalCommand runs the golden set against the live retrieval path and reports
// recall@k, MRR, and latency, optionally diffing against a saved baseline.
func evalCommand(rag *ragpkg.RAG) *cobra.Command {
	var setPath string
	var baselinePath string
	var saveBaseline bool
	var label string
	var maxDistance float64
	var withRerank bool
	var withRewrite bool

	command := &cobra.Command{
		Use:   "eval",
		Short: "Score retrieval quality against the golden set",
		Long: "Runs every question in the golden set through the real retrieval path and reports " +
			"recall@k, MRR, and query latency. With a saved baseline it also shows the delta, so a " +
			"retrieval change can be judged by its effect rather than by feel.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			set, err := eval.Load(setPath)
			if err != nil {
				return err
			}

			// Stages are opted into per run, so the same golden set scores the plain
			// pipeline and each enhanced one — which is the only way to say whether a
			// stage earned its latency.
			var stages []string
			if withRewrite {
				rag = rag.WithRewriter(rag.NewLLMRewriter())
				stages = append(stages, "rewrite")
			}
			if withRerank {
				rag = rag.WithReranker(rag.NewLLMReranker())
				stages = append(stages, "rerank")
			}
			if len(stages) == 0 {
				stages = append(stages, "baseline hybrid")
			}
			fmt.Printf("pipeline: %s\n", strings.Join(stages, " + "))

			// The retriever returns each hit's document identity, which is what the golden
			// set matches on — the origin a file was added from when known, else its path.
			retrieve := func(question string, topK int) ([]string, error) {
				chunks, err := rag.Retrieve(question, maxDistance, topK)
				if err != nil {
					return nil, err
				}
				sources := make([]string, 0, len(chunks))
				for _, chunk := range chunks {
					source := chunk.Source
					if chunk.Origin != "" {
						source = chunk.Origin
					}
					sources = append(sources, source)
				}
				return sources, nil
			}

			report, err := eval.Run(set, retrieve, eval.DefaultKs)
			if err != nil {
				return err
			}

			baseline, hasBaseline, err := eval.LoadBaseline(baselinePath)
			if err != nil {
				return err
			}
			report.Write(os.Stdout, baseline, hasBaseline)

			if saveBaseline {
				if err := os.MkdirAll(filepath.Dir(baselinePath), 0755); err != nil {
					return err
				}
				if err := eval.SaveBaseline(baselinePath, report.ToBaseline(label)); err != nil {
					return err
				}
				fmt.Printf("\nBaseline written to %s\n", baselinePath)
			}
			return nil
		},
	}

	command.Flags().StringVar(&setPath, "set", defaultGoldenSet, "path to the golden set JSON")
	command.Flags().StringVar(&baselinePath, "baseline", defaultBaseline, "path to the baseline JSON")
	command.Flags().BoolVar(&saveBaseline, "save", false, "overwrite the baseline with this run's numbers")
	command.Flags().StringVar(&label, "label", "", "label recorded alongside a saved baseline")
	command.Flags().Float64Var(&maxDistance, "max-distance", ragpkg.DefaultSearchDistance,
		"maximum cosine distance for a vector hit to count")
	command.Flags().BoolVar(&withRerank, "rerank", false, "score with the reranking stage enabled")
	command.Flags().BoolVar(&withRewrite, "rewrite", false, "score with query rewriting enabled")
	return command
}

// reindexCommand re-embeds every stored chunk. Required after the embedding scheme
// changes, since old vectors and new query vectors aren't comparable.
func reindexCommand(rag *ragpkg.RAG) *cobra.Command {
	return &cobra.Command{
		Use:   "reindex",
		Short: "Re-embed every stored chunk and rebuild the vector index",
		Long: "Re-embeds all stored chunks from the text already in the database and rebuilds the " +
			"graph edges. Run this after upgrading Engrex when the embedding scheme has changed — " +
			"vectors written by the old scheme can't be compared against new query vectors. Chunk " +
			"ids, sources, and metadata are preserved; nothing is re-read from disk.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			return rag.Reindex(os.Stdout)
		},
	}
}

// askCommand runs a query in-process with the optional pipeline stages enabled.
//
// Separate from `query`, which goes over the socket to the daemon: these stages are
// experimental and per-invocation, and threading a matrix of flags through the daemon
// protocol would fix a configuration the daemon owns rather than one the caller
// chooses. Running in-process keeps them a property of the question being asked.
func askCommand(rag *ragpkg.RAG) *cobra.Command {
	var withRerank bool
	var withRewrite bool
	var withVerify bool
	var allStages bool
	var maxDistance float64
	var topK int
	var model string

	command := &cobra.Command{
		Use:   "ask [question]",
		Short: "Ask a question with the advanced retrieval stages enabled",
		Long: "Runs a query in-process with reranking, query rewriting, and citation verification " +
			"available as opt-in stages. Unlike `query`, this does not go through the daemon, so " +
			"each stage can be switched on per question to see what it changes.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if allStages {
				withRerank, withRewrite, withVerify = true, true, true
			}
			// Applied before the stages are built, so they inherit the same model
			// rather than the one the instance was constructed with.
			rag = rag.WithModel(model)
			fmt.Fprintf(os.Stderr, "model: %s\n", rag.GenerateModel())

			if withRewrite {
				rag = rag.WithRewriter(rag.NewLLMRewriter())
			}
			if withRerank {
				rag = rag.WithReranker(rag.NewLLMReranker())
			}
			if withVerify {
				rag = rag.WithVerifier(rag.NewLLMVerifier())
			}
			return rag.Query(os.Stdout, args[0], maxDistance, topK)
		},
	}

	command.Flags().BoolVar(&withRerank, "rerank", false, "rerank retrieved passages before answering")
	command.Flags().BoolVar(&withRewrite, "rewrite", false, "decompose multi-part questions before retrieving")
	command.Flags().BoolVar(&withVerify, "verify", false, "check the answer against the retrieved passages")
	command.Flags().BoolVar(&allStages, "all", false, "enable every stage")
	command.Flags().Float64Var(&maxDistance, "max-distance", ragpkg.DefaultSearchDistance,
		"maximum cosine distance for a vector hit to count")
	command.Flags().IntVar(&topK, "top-k", ragpkg.DefaultSearchResults, "passages to put in the prompt")
	command.Flags().StringVar(&model, "model", "",
		"Ollama model to generate with (overrides config and ENGREX_GENERATE_MODEL)")
	return command
}

// debugPromptCommand prints the exact prompt a query would send. Separates "the model
// was given the wrong context" from "the model was given the right context and still
// answered badly" — two failures that look identical from the answer alone.
func debugPromptCommand(rag *ragpkg.RAG) *cobra.Command {
	return &cobra.Command{
		Use:   "debug-prompt [question]",
		Short: "Print the exact prompt a query would send to the LLM",
		Long: "Runs retrieval for a question and prints the fully assembled prompt without generating " +
			"an answer. Use it to check what the model actually saw before blaming the model.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			prompt, err := rag.DebugPrompt(args[0], ragpkg.DefaultSearchDistance, ragpkg.DefaultSearchResults)
			if err != nil {
				return err
			}
			fmt.Println(prompt)
			return nil
		},
	}
}

// doctorCommand reports the diagnostics behind the retrieval-correctness findings:
// whether the embedding model returns normalized vectors, and what schema version the
// database is at.
func doctorCommand(rag *ragpkg.RAG) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check embedding and index health",
		Long: "Reports the embedding model's raw vector magnitude (which determines whether L2 and " +
			"cosine distance rank identically), the database schema version, and how many chunks " +
			"are indexed.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			report, err := rag.Diagnose()
			if err != nil {
				return err
			}

			fmt.Printf("Embedding model:     %s\n", report.Model)
			fmt.Printf("Vector dimensions:   %d\n", report.Dimensions)
			fmt.Printf("Raw magnitude:       %.4f\n", report.RawMagnitude)
			fmt.Printf("Stored magnitude:    %.4f\n", report.StoredMagnitude)
			if report.RawMagnitude > 1.01 || report.RawMagnitude < 0.99 {
				fmt.Println("  → model output is NOT unit length; Engrex normalizes before storing,")
				fmt.Println("    which is what keeps cosine distance well behaved.")
			} else {
				fmt.Println("  → model output is already unit length.")
			}

			fmt.Printf("Schema version:      %d\n", report.SchemaVersion)
			fmt.Printf("Chunks indexed:      %d\n", report.ChunkCount)
			if report.VectorCount != report.ChunkCount {
				fmt.Printf("Vectors indexed:     %d\n", report.VectorCount)
				fmt.Println("  → vector count does not match chunk count; run `engrex reindex`.")
			}
			return nil
		},
	}
}
