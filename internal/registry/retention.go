package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// PruneOptions describes which pseudo-versions may be removed.
type PruneOptions struct {
	KeepNewest int
	MaxAge     time.Duration
	Now        time.Time
	DryRun     bool
}

// Pruned reports what a prune removed, or would remove.
type Pruned struct {
	Module  Module
	Version string
	Reason  string
}

// PrunePseudoVersions deletes pseudo-versions beyond the retention policy.
//
// Only pseudo-versions are eligible: they are produced by CI on every push and
// documented as not for production. Tagged versions and anything a label
// points at stay, because a consumer may have pinned them.
func (s *Store) PrunePseudoVersions(opts PruneOptions) ([]Pruned, error) {
	if opts.KeepNewest <= 0 && opts.MaxAge <= 0 {
		return nil, nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	mods, err := s.ListModules()
	if err != nil {
		return nil, err
	}
	var pruned []Pruned
	for _, m := range mods {
		mu := s.lock(m)
		mu.Lock()
		removed, err := s.prunePseudoLocked(m, opts, now)
		mu.Unlock()
		if err != nil {
			return pruned, err
		}
		pruned = append(pruned, removed...)
	}
	return pruned, nil
}

func (s *Store) prunePseudoLocked(m Module, opts PruneOptions, now time.Time) ([]Pruned, error) {
	idx, err := s.Index(m)
	if err != nil {
		return nil, err
	}
	labelled := map[string]bool{}
	for _, target := range idx.Labels {
		labelled[target] = true
	}

	// Versions are sorted ascending, so walk from the newest backwards and
	// count the pseudo-versions worth keeping.
	kept := 0
	var doomed []Version
	for i := len(idx.Versions) - 1; i >= 0; i-- {
		v := idx.Versions[i]
		if !IsPseudo(v.Version) || labelled[v.Version] {
			continue
		}
		reason := ""
		switch {
		case opts.KeepNewest > 0 && kept >= opts.KeepNewest:
			reason = fmt.Sprintf("beyond the newest %d pseudo-versions", opts.KeepNewest)
		case opts.MaxAge > 0 && now.Sub(v.CreatedAt) > opts.MaxAge:
			reason = fmt.Sprintf("older than %s", opts.MaxAge)
		}
		if reason == "" {
			kept++
			continue
		}
		v.YankReason = reason // reused as the prune reason for reporting
		doomed = append(doomed, v)
	}
	if len(doomed) == 0 {
		return nil, nil
	}

	var pruned []Pruned
	remaining := make([]Version, 0, len(idx.Versions)-len(doomed))
	drop := map[string]string{}
	for _, v := range doomed {
		drop[v.Version] = v.YankReason
	}
	for _, v := range idx.Versions {
		if _, ok := drop[v.Version]; !ok {
			remaining = append(remaining, v)
		}
	}
	if opts.DryRun {
		for _, v := range doomed {
			pruned = append(pruned, Pruned{Module: m, Version: v.Version, Reason: drop[v.Version]})
		}
		return pruned, nil
	}

	// Update the index first: a version nobody references is an orphan the
	// next publish or gc removes, while a file missing under a live index
	// entry is a 500 for whoever downloads it.
	idx.Versions = remaining
	idx.Updated = now.UTC()
	if err := s.writeIndex(m, idx); err != nil {
		return nil, err
	}
	for _, v := range doomed {
		if err := os.RemoveAll(filepath.Join(s.moduleDir(m), "versions", v.Version)); err != nil {
			return pruned, err
		}
		pruned = append(pruned, Pruned{Module: m, Version: v.Version, Reason: drop[v.Version]})
	}
	return pruned, nil
}
