package cli

// vornikctl models profile list|apply and vornikctl models tiers (GitHub
// #880, project and swarm registry design §11.2 R6). The daemon does the
// work through its admin API; these commands render its answers.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type modelChoiceCLI struct {
	Model    string `json:"model"`
	Fallback string `json:"fallback,omitempty"`
}

type modelProfileCLI struct {
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Matches     bool                      `json:"matches_endpoint"`
	Tiers       map[string]modelChoiceCLI `json:"tiers"`
	Roles       map[string]modelChoiceCLI `json:"roles"`
}

type modelProfileApplyCLI struct {
	Name            string                    `json:"name"`
	Tiers           map[string]modelChoiceCLI `json:"tiers"`
	Roles           map[string]modelChoiceCLI `json:"roles"`
	Warnings        []string                  `json:"warnings"`
	Applied         bool                      `json:"applied"`
	RestartRequired bool                      `json:"restart_required"`
}

type roleTierCLI struct {
	Swarm    string `json:"swarm"`
	Role     string `json:"role"`
	Tier     string `json:"tier"`
	Model    string `json:"model"`
	Fallback string `json:"fallback"`
	Source   string `json:"source"`
}

var modelsProfileApply, modelsProfileForce bool

var modelsProfileCmd = &cobra.Command{
	Use:   "profile",
	Short: "List or apply a provider profile that maps model tiers to models",
	Long: `A provider profile maps the model tiers shipped roles name (trivial,
standard, complex, coding, vision) to one provider's model IDs. Applying it
writes runtime.agent_llm.tiers and runtime.agent_llm.roles in config.yaml;
after that they are your instance's config. A tier with no mapping runs on
the setup model.`,
}

var modelsProfileListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the shipped provider profiles",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		var out struct {
			Profiles []modelProfileCLI `json:"profiles"`
		}
		if err := modelsAPI(http.MethodGet, "/api/v1/models/profiles", nil, &out); err != nil {
			return err
		}
		fmt.Print(renderModelProfiles(out.Profiles))
		return nil
	},
}

var modelsProfileApplyCmd = &cobra.Command{
	Use:   "apply <name>",
	Short: "Show (or, with --apply, write) a provider profile's tier mappings",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		var out modelProfileApplyCLI
		body := map[string]bool{"apply": modelsProfileApply, "force": modelsProfileForce}
		if err := modelsAPI(http.MethodPost, "/api/v1/models/profiles/"+args[0]+"/apply", body, &out); err != nil {
			return err
		}
		fmt.Print(renderModelProfileApply(out))
		return nil
	},
}

var modelsTiersCmd = &cobra.Command{
	Use:   "tiers",
	Short: "Show each role's model tier and the model it resolves to",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		var out struct {
			Roles []roleTierCLI `json:"roles"`
		}
		if err := modelsAPI(http.MethodGet, "/api/v1/models/tiers", nil, &out); err != nil {
			return err
		}
		fmt.Print(renderModelTiers(out.Roles))
		return nil
	},
}

func init() {
	modelsProfileApplyCmd.Flags().BoolVar(&modelsProfileApply, "apply", false, "Write the mappings (default: dry run)")
	modelsProfileApplyCmd.Flags().BoolVar(&modelsProfileForce, "force", false, "Write without checking that each model answers (audited)")
	modelsProfileCmd.AddCommand(modelsProfileListCmd, modelsProfileApplyCmd)
	modelsCmd.AddCommand(modelsProfileCmd, modelsTiersCmd)
}

// modelsAPI calls the daemon and decodes a 200 answer into out.
func modelsAPI(method, path string, body any, out any) error {
	client := ClientFromEnv()
	var (
		resp *http.Response
		err  error
	)
	if method == http.MethodPost {
		resp, err = client.Post(path, body)
	} else {
		resp, err = client.Get(path)
	}
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}
	return nil
}

func choiceText(c modelChoiceCLI) string {
	if c.Fallback == "" {
		return c.Model
	}
	return c.Model + " (fallback " + c.Fallback + ")"
}

func writeChoices(b *bytes.Buffer, indent string, m map[string]modelChoiceCLI) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s%-30s %s\n", indent, k, choiceText(m[k]))
	}
}

func renderModelProfiles(profiles []modelProfileCLI) string {
	var b bytes.Buffer
	if len(profiles) == 0 {
		b.WriteString("No provider profiles are shipped in this install.\n")
		return b.String()
	}
	match := ""
	for _, p := range profiles {
		head := p.Name
		if p.Matches {
			head += "   <- matches your endpoint"
			match = p.Name
		}
		fmt.Fprintf(&b, "%s\n  %s\n", head, p.Description)
		writeChoices(&b, "    ", p.Tiers)
		writeChoices(&b, "    ", p.Roles)
		b.WriteString("\n")
	}
	if match != "" {
		fmt.Fprintf(&b, "Preview it with: vornikctl models profile apply %s\n", match)
	} else {
		b.WriteString("No profile matches your endpoint; unmapped tiers run on the setup model.\n")
	}
	return b.String()
}

func renderModelProfileApply(res modelProfileApplyCLI) string {
	var b bytes.Buffer
	if !res.Applied {
		fmt.Fprintf(&b, "Dry run: applying %q replaces these two keys in config.yaml.\n", res.Name)
	} else {
		fmt.Fprintf(&b, "Applied %q to config.yaml.\n", res.Name)
	}
	b.WriteString("runtime.agent_llm.tiers:\n")
	writeChoices(&b, "  ", res.Tiers)
	b.WriteString("runtime.agent_llm.roles:\n")
	writeChoices(&b, "  ", res.Roles)
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "Warning: %s\n", w)
	}
	switch {
	case !res.Applied:
		fmt.Fprintf(&b, "Write it with: vornikctl models profile apply %s --apply\n", res.Name)
	case res.RestartRequired:
		b.WriteString("Restart the daemon to use it: runtime.agent_llm is read at start.\n")
	}
	return b.String()
}

func renderModelTiers(roles []roleTierCLI) string {
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SWARM\tROLE\tTIER\tMODEL\tFALLBACK\tSOURCE")
	for _, r := range roles {
		model, source := r.Model, r.Source
		if model == "" {
			model, source = "(setup model)", "inherits"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Swarm, r.Role, dash(r.Tier), model, dash(r.Fallback), source)
	}
	_ = tw.Flush()
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
