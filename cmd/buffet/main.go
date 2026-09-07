// Command buffet publishes protobuf schemas to a protobuffet registry and
// checks them for backwards compatibility.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
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
  buffet login [flags]            store a token for a registry
  buffet token                    generate a token and its config digest
  buffet version                  print the version

common flags:
  -registry URL   registry root (env BUFFET_REGISTRY)
  -token TOKEN    API token (env BUFFET_TOKEN, or buffet login)
  -config PATH    module config, default ./buffet.yaml
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "publish":
		err = cmdPublish(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "info":
		err = cmdInfo(os.Args[2:])
	case "login":
		err = cmdLogin(os.Args[2:])
	case "token":
		err = cmdToken()
	case "version", "--version", "-version":
		fmt.Println("buffet", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "buffet:", err)
		os.Exit(1)
	}
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
	if token == "" {
		token = storedToken(url)
	}
	return client.New(url, token), nil
}

func cmdPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	configPath := fs.String("config", ".", "module config file or directory")
	versionFlag := fs.String("version", "", "version to publish, default: git tag or a pseudo-version")
	label := fs.String("label", "", "label to move to this version, for example main")
	skipGenerate := fs.Bool("skip-generate", false, "publish existing generated output without running buf generate")
	skipLint := fs.Bool("skip-lint", false, "skip buf lint")
	dryRun := fs.Bool("dry-run", false, "build artifacts and print what would be pushed")
	allowBreaking := fs.Bool("allow-breaking", false, "publish even if the change breaks consumers (admin token only)")
	bufPath := fs.String("buf", "", "path to the buf binary")
	fs.Parse(args)

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

	fmt.Printf("module   %s\nversion  %s\nschema   %d bytes (sha256 %s)\n",
		build.Module, build.Version, len(build.SchemaZip), registry.SHA256Hex(build.SchemaZip)[:12])
	for _, a := range build.Artifacts {
		fmt.Printf("target   %-9s %s@%s (%d bytes)\n", a.Target, a.Name, a.Version, len(build.Files[a.File]))
	}
	if *dryRun {
		fmt.Println("\ndry run: nothing was pushed")
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
	resp, ok, err := c.Publish(man, build.Files)
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
	fmt.Printf("\npublished %s@%s\n", resp.Module, resp.Version.Version)
	if b := resp.Breaking; b != nil {
		switch {
		case b.Overridden != "":
			fmt.Printf("compatibility: BREAKING against %s, published anyway (%s)\n", b.Against, b.Overridden)
		case b.Checked:
			fmt.Printf("compatibility: checked against %s, no breaking changes\n", b.Against)
		case b.Skipped != "":
			fmt.Printf("compatibility: not checked (%s)\n", b.Skipped)
		}
	}
	return nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	configPath := fs.String("config", ".", "module config file or directory")
	against := fs.String("against", "latest", "version or label to compare against")
	bufPath := fs.String("buf", "", "path to the buf binary")
	fs.Parse(args)

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

	prev, err := c.DownloadSchema(m.Module, *against)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "buffet-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	prevPath := filepath.Join(dir, "previous.zip")
	if err := os.WriteFile(prevPath, prev, 0o600); err != nil {
		return err
	}

	res, err := buf.Breaking(context.Background(), m.Dir, "", prevPath)
	if err != nil {
		return err
	}
	if res.Compatible {
		fmt.Printf("compatible with %s@%s\n", m.Module, *against)
		return nil
	}
	fmt.Println(res.Output)
	return fmt.Errorf("breaking changes against %s@%s", m.Module, *against)
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	fs.Parse(args)

	c, err := rf.client()
	if err != nil {
		return err
	}
	mods, err := c.ListModules()
	if err != nil {
		return err
	}
	if len(mods) == 0 {
		fmt.Println("no modules published yet")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODULE\tLATEST\tVERSIONS\tUPDATED")
	for _, m := range mods {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", m.Module, m.Latest, m.Versions, m.Updated.Format(time.DateOnly))
	}
	return w.Flush()
}

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: buffet info <owner/name>")
	}
	c, err := rf.client()
	if err != nil {
		return err
	}
	idx, err := c.Module(fs.Arg(0))
	if err != nil {
		return err
	}
	labels := map[string][]string{}
	for name, v := range idx.Labels {
		labels[v] = append(labels[v], name)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tPUBLISHED\tTARGETS\tLABELS")
	for i := len(idx.Versions) - 1; i >= 0; i-- {
		v := idx.Versions[i]
		var targets []string
		for _, t := range registry.AllTargets {
			if a, ok := v.Artifact(t); ok {
				targets = append(targets, fmt.Sprintf("%s:%s", t, a.Name))
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			v.Version, v.CreatedAt.Format(time.RFC3339), strings.Join(targets, " "), strings.Join(labels[v.Version], ","))
	}
	return w.Flush()
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	var rf registryFlags
	addRegistryFlags(fs, &rf)
	fs.Parse(args)

	url := rf.registry
	if url == "" {
		url = os.Getenv("BUFFET_REGISTRY")
	}
	if url == "" || rf.token == "" {
		return fmt.Errorf("usage: buffet login -registry URL -token TOKEN")
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
	fmt.Printf("token stored in %s\n", path)
	return nil
}

// cmdToken mints a token and prints both halves: the secret for the client and
// the digest for the server config, so nobody has to hash it by hand.
func cmdToken() error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	fmt.Printf("token:  %s\n\nadd to buffetd.yaml:\n\ntokens:\n  - subject: ci\n    sha256: %s\n    scopes: [write]\n",
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
	json.Unmarshal(b, &out)
	return out
}

func storedToken(url string) string {
	return loadCredentials()[strings.TrimRight(url, "/")]
}
