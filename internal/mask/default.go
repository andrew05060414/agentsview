package mask

import (
	"log"
	"os"
	"strings"
	"sync"
)

// Environment switches for the ingestion-time masker (M1).
//
//	AGENTSVIEW_MASK=off            disable masking of newly stored text
//	AGENTSVIEW_MASK_REGISTRY=path  hashed known-secret registry (JSONL, see Registry)
//	AGENTSVIEW_MASK_SECRETS_DIR=d  directory of plaintext secret files; strict values
//	                               are held in memory only and replaced wherever they appear
const (
	EnvMask       = "AGENTSVIEW_MASK"
	EnvRegistry   = "AGENTSVIEW_MASK_REGISTRY"
	EnvSecretsDir = "AGENTSVIEW_MASK_SECRETS_DIR"
)

var (
	defaultOnce   sync.Once
	defaultMasker *Masker
)

// Default returns the process-wide ingestion masker, or nil when masking is
// switched off or the rules cannot be loaded (a broken rule file must not stop
// ingestion; it is logged once). It is built on first use.
func Default() *Masker {
	defaultOnce.Do(func() {
		if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvMask)), "off") {
			return
		}
		var opts Options
		if p := os.Getenv(EnvRegistry); p != "" {
			reg, err := LoadRegistry(p)
			if err != nil {
				log.Printf("mask: ignoring registry: %v", err)
			} else {
				opts.Registry = reg
			}
		}
		if d := os.Getenv(EnvSecretsDir); d != "" {
			vals, err := LoadExactDir(d)
			if err != nil {
				log.Printf("mask: ignoring secrets dir: %v", err)
			}
			for _, v := range vals {
				if v.Strict {
					opts.Exact = append(opts.Exact, v)
				}
			}
		}
		m, err := NewDefault(opts)
		if err != nil {
			log.Printf("mask: disabled, rules failed to load: %v", err)
			return
		}
		defaultMasker = m
	})
	return defaultMasker
}

// SetDefaultForTest replaces the process-wide masker and returns a restore
// function. Only tests may call it.
func SetDefaultForTest(m *Masker) (restore func()) {
	defaultOnce.Do(func() {})
	prev := defaultMasker
	defaultMasker = m
	return func() { defaultMasker = prev }
}
