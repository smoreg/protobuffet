// Command buffet publishes protobuf schemas to a protobuffet registry and
// checks them for backwards compatibility.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/smoreg/protobuffet/internal/api"
	"github.com/smoreg/protobuffet/internal/buftool"
	"github.com/smoreg/protobuffet/internal/client"
	"github.com/smoreg/protobuffet/internal/config"
	"github.com/smoreg/protobuffet/internal/publish"
	"github.com/smoreg/protobuffet/internal/registry"
)

var version = "dev"

const usage = `buffet - publish protobuf schemas to a protobuffet registry

usage:
  buffet publish [flags]          generate SDKs and push a new version
  buffet check [flags]            check compatibility against the registry
  buffet list [flags]             list modules in the registry
  buffet info <owner/name> [flags]  show versions of one module
  buffet vendor [flags]           fetch dependency schemas from the registry
  buffet label <module> <label> <version>  move a label
  buffet yank <module>@<version>  hide a bad release (admin, keeps it downloadable)
  buffet unyank <module>@<version>  undo a yank
  buffet login [flags]            store a token for a registry
  buffet whoami                   show what the configured token can do
  buffet token                    generate a token and its config digest
  buffet version                  print the version

common flags:
  -registry URL   registry root (env BUFFET_REGISTRY)
  -token TOKEN    API token (env BUFFET_TOKEN, BUFFET_TOKEN_FILE, or buffet login)
  -config PATH    module config, default ./buffet.yaml
  -json           machine-readable output (list, info, whoami, publish)
`

func main() {
	code := run(os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(code)
}

// ErrUsage asks the caller to exit with the usage status.
var errUsage = errors.New("usage")

// run executes one command and returns the process exit code. Keeping main to
// three lines makes the commands testable without spawning a binary.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage) //nolint:errcheck
		return 2
	}
	var err error
	switch args[0] {
	case "publish":
		err = cmdPublish(args[1:], stdout)
	case "check":
		err = cmdCheck(args[1:], stdout)
	case "list":
		err = cmdList(args[1:], stdout)
	case "info":
		err = cmdInfo(args[1:], stdout)
	case "vendor":
		err = cmdVendor(args[1:], stdout)
	case "label":
		err = cmdLabel(args[1:], stdout)
	case "yank":
		err = cmdYank(args[1:], stdout, true)
	case "unyank":
		err = cmdYank(args[1:], stdout, false)
	case "login":
		err = cmdLogin(args[1:], stdout)
	case "whoami":
		err = cmdWhoami(args[1:], stdout)
	case "token":
		err = cmdToken(stdout)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "buffet", version) //nolint:errcheck
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage) //nolint:errcheck
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage) //nolint:errcheck
		return 2
	}
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprint(stderr, usage) //nolint:errcheck
		return 2
	case err != nil:
		fmt.Fprintln(stderr, "buffet:", err) //nolint:errcheck
		return 1
	}
	return 0
}

// registryFlags are shared by every command that talks to a registry.
type registryFlags struct {
	registry string
	token    string
}

func addRegistryFlags(fs *flag.FlagSet, f *registryFlags) {
	fs.StringVar(&f.registry, "registry", "", "registry root URL (env BUFFET_REGISTRY)")
	fs.StringVar(&f.token, "token", "", "API token (env BUFFET_TOKEN)")
}

func (f *registryFlags) client() (*client.Client, error) {
	url := f.registry
	if url == "" {
		url = os.Getenv("BUFFET_REGISTRY")
	}
	if url == "" {
		return nil, fmt.Errorf("no registry: pass -registry or set BUFFET_REGISTRY")
	}
	token := f.token
	if token == "" {
		token = os.Getenv("BUFFET_TOKEN")
	}
	// A file keeps the credential out of the process environment, which is
	// what a mounted Kubernetes secret gives you for free.
	if token == "" {
		if path := os.Getenv("BUFFET_TOKEN_FILE"); path != "" {
			body, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read BUFFET_TOKEN_FILE: %w", err)
			}
			token = strings.TrimSpace(string(body))
		}
	}
	if token == "" {
		token = storedToken(url)
	}
	return client.New(url, token), nil
}

func cmdPublish(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	configPath := fs.String("config", ".", "module config file or directory")
	versionFlag := fs.String("version", "", "version to publish, default: git tag or a pseudo-version")
	label := fs.String("label", "", "label to move to this version, for example main")
	skipGenerate := fs.Bool("skip-generate", false, "publish existing generated output without running buf generate")
	skipLint := fs.Bool("skip-lint", false, "skip buf lint")
	dryRun := fs.Bool("dry-run", false, "build artifacts and print what would be pushed")
	asJSON := fs.Bool("json", false, "machine-readable output")
	allowBreaking := fs.Bool("allow-breaking", false, "publish even if the change breaks consumers (admin token only)")
	bufPath := fs.String("buf", "", "path to the buf binary")
	_ = fs.Parse(args) // ExitOnError: a parse failure has already exited

	m, err := config.LoadModule(*configPath)
	if err != nil {
		return err
	}
	buf, bufErr := buftool.Find(*bufPath)
	if bufErr != nil && !*skipGenerate {
		return fmt.Errorf("%w (install buf, or pass -skip-generate)", bufErr)
	}

	ctx := context.Background()
	if !*skipLint && buf != nil {
		if err := buf.Lint(ctx, m.Dir); err != nil {
			return err
		}
	}

	build, err := publish.Prepare(ctx, buf, m, publish.Options{
		Version:      *versionFlag,
		SkipGenerate: *skipGenerate,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "module   %s\nversion  %s\nschema   %d bytes (sha256 %s)\n", //nolint:errcheck
		build.Module, build.Version, len(build.SchemaZip), registry.SHA256Hex(build.SchemaZip)[:12])
	for _, a := range build.Artifacts {
		fmt.Fprintf(stdout, "target   %-9s %s@%s (%d bytes)\n", a.Target, a.Name, a.Version, len(build.Files[a.File])) //nolint:errcheck
	}
	if *dryRun {
		fmt.Fprintln(stdout, "\ndry run: nothing was pushed") //nolint:errcheck
		return nil
	}

	c, err := rf.client()
	if err != nil {
		return err
	}
	man := api.PublishManifest{
		Module:       build.Module,
		Version:      build.Version,
		Artifacts:    build.Artifacts,
		SchemaSHA256: registry.SHA256Hex(build.SchemaZip),
	}
	if *label != "" {
		man.Labels = []string{*label}
	}
	man.AllowBreaking = *allowBreaking
	resp, ok, err := c.Publish(ctx, man, build.Files)
	if err != nil {
		return err
	}
	if !ok {
		if resp.Breaking != nil && resp.Breaking.Findings != "" {
			return fmt.Errorf("registry refused the push: it breaks compatibility with %s\n\n%s",
				resp.Breaking.Against, resp.Breaking.Findings)
		}
		return fmt.Errorf("registry refused the push")
	}
	if *asJSON {
		return writeJSON(stdout, resp)
	}
	fmt.Fprintf(stdout, "\npublished %s@%s\n", resp.Module, resp.Version.Version) //nolint:errcheck
	if b := resp.Breaking; b != nil {
		switch {
		case b.Overridden != "":
			fmt.Fprintf(stdout, "compatibility: BREAKING against %s, published anyway (%s)\n", b.Against, b.Overridden) //nolint:errcheck
		case b.Checked:
			fmt.Fprintf(stdout, "compatibility: checked against %s, no breaking changes\n", b.Against) //nolint:errcheck
		case b.Skipped != "":
			fmt.Fprintf(stdout, "compatibility: not checked (%s)\n", b.Skipped) //nolint:errcheck
		}
	}
	return nil
}

func cmdCheck(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	configPath := fs.String("config", ".", "module config file or directory")
	against := fs.String("against", "latest", "version or label to compare against")
	bufPath := fs.String("buf", "", "path to the buf binary")
	_ = fs.Parse(args) // ExitOnError: a parse failure has already exited

	m, err := config.LoadModule(*configPath)
	if err != nil {
		return err
	}
	buf, err := buftool.Find(*bufPath)
	if err != nil {
		return err
	}
	c, err := rf.client()
	if err != nil {
		return err
	}
	ctx := context.Background()

	prev, err := c.DownloadSchema(ctx, m.Module, *against)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "buffet-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup of a temp dir
	prevPath := filepath.Join(dir, "previous.zip")
	if err := os.WriteFile(prevPath, prev, 0o600); err != nil {
		return err
	}

	res, err := buf.Breaking(ctx, m.Dir, "", prevPath)
	if err != nil {
		return err
	}
	if res.Compatible {
		fmt.Fprintf(stdout, "compatible with %s@%s\n", m.Module, *against) //nolint:errcheck
		return nil
	}
	fmt.Fprintln(stdout, res.Output) //nolint:errcheck
	return fmt.Errorf("breaking changes against %s@%s", m.Module, *against)
}

func cmdWhoami(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("whoami", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args) // ExitOnError: a parse failure has already exited

	c, err := rf.client()
	if err != nil {
		return err
	}
	id, err := c.Whoami(context.Background())
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, id)
	}
	fmt.Fprintf(stdout, "subject  %s\nscopes   %s\nmodules  %s\n", //nolint:errcheck
		id.Subject, strings.Join(id.Scopes, ", "), strings.Join(id.Modules, ", "))
	return nil
}

// writeJSON prints a value for scripts to consume.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdList(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args) // ExitOnError: a parse failure has already exited

	c, err := rf.client()
	if err != nil {
		return err
	}
	mods, err := c.ListModules(context.Background())
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, mods)
	}
	if len(mods) == 0 {
		fmt.Fprintln(stdout, "no modules published yet") //nolint:errcheck
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODULE\tLATEST\tVERSIONS\tUPDATED") //nolint:errcheck // buffered by tabwriter, Flush reports the error
	for _, m := range mods {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", m.Module, m.Latest, m.Versions, m.Updated.Format(time.DateOnly)) //nolint:errcheck
	}
	return w.Flush()
}

func cmdInfo(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args) // ExitOnError: a parse failure has already exited
	if fs.NArg() != 1 {
		return fmt.Errorf("info takes one module: %w", errUsage)
	}
	c, err := rf.client()
	if err != nil {
		return err
	}
	idx, err := c.Module(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, idx)
	}
	labels := map[string][]string{}
	for name, v := range idx.Labels {
		labels[v] = append(labels[v], name)
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tPUBLISHED\tTARGETS\tLABELS") //nolint:errcheck // buffered by tabwriter, Flush reports the error
	for i := len(idx.Versions) - 1; i >= 0; i-- {
		v := idx.Versions[i]
		var targets []string
		for _, t := range registry.AllTargets {
			if a, ok := v.Artifact(t); ok {
				targets = append(targets, fmt.Sprintf("%s:%s", t, a.Name))
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", //nolint:errcheck
			v.Version, v.CreatedAt.Format(time.RFC3339), strings.Join(targets, " "), strings.Join(labels[v.Version], ","))
	}
	return w.Flush()
}

func cmdVendor(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("vendor", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	configPath := fs.String("config", ".", "module config file or directory")
	_ = fs.Parse(args) // ExitOnError: a parse failure has already exited

	m, err := config.LoadModule(*configPath)
	if err != nil {
		return err
	}
	if len(m.Deps) == 0 {
		fmt.Fprintln(stdout, "no deps declared in buffet.yaml, nothing to vendor") //nolint:errcheck
		return nil
	}
	c, err := rf.client()
	if err != nil {
		return err
	}
	vendored, err := publish.Vendor(context.Background(), c, m)
	if err != nil {
		return err
	}
	for _, d := range vendored {
		rel, relErr := filepath.Rel(m.Dir, d.Path)
		if relErr != nil {
			rel = d.Path
		}
		fmt.Fprintf(stdout, "%-24s %-12s %2d files -> %s\n", d.Module, d.Version, d.Files, rel) //nolint:errcheck
	}
	fmt.Fprintf(stdout, "\nwrote buf.vendor.yaml; generate with:\n  buf generate --config buf.vendor.yaml\n") //nolint:errcheck
	fmt.Fprintf(stdout, "add to .gitignore:\n  %s", publish.VendorGitignore(m))                               //nolint:errcheck
	return nil
}

func cmdLabel(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("label", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	_ = fs.Parse(args)
	if fs.NArg() != 3 {
		return fmt.Errorf("label takes <module> <label> <version>: %w", errUsage)
	}
	c, err := rf.client()
	if err != nil {
		return err
	}
	if err := c.SetLabel(context.Background(), fs.Arg(0), fs.Arg(1), fs.Arg(2)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: label %s now points at %s\n", fs.Arg(0), fs.Arg(1), fs.Arg(2)) //nolint:errcheck
	return nil
}

func cmdYank(args []string, stdout io.Writer, yanked bool) error {
	name := "yank"
	if !yanked {
		name = "unyank"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	reason := fs.String("reason", "", "why the version is being yanked")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("%s takes <module>@<version>: %w", name, errUsage)
	}
	module, version, ok := strings.Cut(fs.Arg(0), "@")
	if !ok {
		return fmt.Errorf("%s takes <module>@<version>: %w", name, errUsage)
	}
	c, err := rf.client()
	if err != nil {
		return err
	}
	if err := c.SetYanked(context.Background(), module, version, yanked, *reason); err != nil {
		return err
	}
	if yanked {
		fmt.Fprintf(stdout, "%s@%s yanked; it stays downloadable for lock files that pin it\n", module, version) //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%s@%s is listed again\n", module, version) //nolint:errcheck
	}
	return nil
}

func cmdLogin(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	_ = fs.Parse(args) // ExitOnError: a parse failure has already exited

	url := rf.registry
	if url == "" {
		url = os.Getenv("BUFFET_REGISTRY")
	}
	if url == "" || rf.token == "" {
		return fmt.Errorf("login needs -registry and -token: %w", errUsage)
	}
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	creds := loadCredentials()
	creds[strings.TrimRight(url, "/")] = rf.token
	body, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "token stored in %s\n", path)                                                  //nolint:errcheck
	fmt.Fprintln(stdout, "it is stored in plain text; in CI prefer BUFFET_TOKEN or BUFFET_TOKEN_FILE") //nolint:errcheck
	return nil
}

// cmdToken mints a token and prints both halves: the secret for the client and
// the digest for the server config, so nobody has to hash it by hand.
func cmdToken(stdout io.Writer) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	fmt.Fprintf(stdout, "token:  %s\n\nadd to buffetd.yaml:\n\ntokens:\n  - subject: ci\n    sha256: %s\n    scopes: [write]\n", //nolint:errcheck
		token, config.HashToken(token))
	return nil
}

func credentialsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "protobuffet", "credentials.json"), nil
}

func loadCredentials() map[string]string {
	out := map[string]string{}
	path, err := credentialsPath()
	if err != nil {
		return out
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	if err := json.Unmarshal(b, &out); err != nil {
		// A corrupt credentials file behaves like no credentials at all.
		return map[string]string{}
	}
	return out
}

func storedToken(url string) string {
	return loadCredentials()[strings.TrimRight(url, "/")]
}
