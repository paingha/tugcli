package cli

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/intelligence"
	"github.com/paingha/tugcli/internal/render"
)

func newIntelligenceCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "intelligence",
		Short: "Set up the model providers behind tug why --deep, and see what they cost",
		Long: `tug why --deep asks a model to explain a diagnosis: Anthropic's Claude,
OpenAI, Mistral, or any OpenAI-compatible endpoint (custom), such as Ollama,
vLLM or LiteLLM. It is off until tug intelligence enable turns it on with a
provider; tug settings changes the rest (model, address, timeout, answer
length and a monthly spend cap), all kept in config.yaml.

API keys live in the credential store, one per provider, or in
TUG_SECRET_<PROVIDER>_API_KEY for scripts and CI; never in config.yaml or a
flag. tug prices every call from a built-in table, which config.yaml can
extend, and keeps a local ledger of what each call took.`,
		Example: `  tug intelligence enable anthropic    # turn --deep on, and store the API key
  tug intelligence enable custom --base-url http://localhost:11434/v1 --model llama4:70b
  tug intelligence key set anthropic   # asks for the key and keeps it in the keychain
  tug intelligence status
  tug intelligence models              # the providers' default models and prices
  tug intelligence usage               # calls, and what they cost by day and month`,
	}
	cmd.AddCommand(newIntelligenceEnableCmd(a), newIntelligenceDisableCmd(a),
		newIntelligenceStatusCmd(a), newIntelligenceUsageCmd(a), newIntelligenceKeyCmd(a), newIntelligenceModelsCmd(a))
	return cmd
}

// intelligenceStatus is what tug intelligence status shows.
type intelligenceStatus struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`
	// Key says where the API key comes from, never what it is.
	Key   string              `json:"key"`
	Price *intelligence.Price `json:"price"`
	// MonthlySpendCapUSD is zero when there is no cap.
	MonthlySpendCapUSD float64            `json:"monthlySpendCapUSD"`
	ThisMonth          intelligence.Total `json:"thisMonth"`
}

func newIntelligenceStatusCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the provider, model and key --deep uses, its price and this month's spend",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			s, err := intelligence.SettingsFrom(a.cfg.Intelligence, intelligence.Overrides{})
			if err != nil {
				return err
			}
			key, source, err := a.intelligenceKey(s.Provider)
			if err != nil {
				return err
			}
			calls, err := intelligence.OpenLedger(a.paths.Data, a.now).Calls()
			if err != nil {
				return err
			}
			st := intelligenceStatus{
				Enabled: s.Enabled, Provider: s.Provider, Model: s.Model,
				Endpoint:           intelligence.NewClient(s, "").Endpoint(),
				MonthlySpendCapUSD: s.MonthlySpendCapUSD,
				ThisMonth:          intelligence.MonthSpend(calls, a.now()),
			}
			switch {
			case key != "":
				st.Key = source
			case intelligence.KeyRequired(s.Provider):
				st.Key = "none"
			default:
				st.Key = "none needed"
			}
			if p, ok := s.Prices.For(s.Model); ok {
				st.Price = &p
			}
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, st)
			}
			return a.printIntelligenceStatus(st)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func (a *app) printIntelligenceStatus(st intelligenceStatus) error {
	c := a.colors()
	enabled := c.Yellow("no; turn tug why --deep on with `tug intelligence enable`")
	if st.Enabled {
		enabled = c.Green("yes")
	}
	key := st.Key
	switch {
	case key == "none":
		key = c.Yellow("none; run `tug intelligence key set " + st.Provider + "`, or set " + intelligence.KeyEnv(st.Provider))
	case strings.HasPrefix(key, "TUG_SECRET_"):
		key = "from " + key
	case key != "none needed":
		key = "stored in " + key
	}
	price := c.Yellow("unknown; add one under intelligence.prices in config.yaml")
	if p := st.Price; p != nil {
		price = priceText(*p)
	}
	month := fmt.Sprintf("%s in %d calls", dollars(st.ThisMonth.CostUSD), st.ThisMonth.Calls)
	if st.ThisMonth.UnpricedCalls > 0 {
		month += fmt.Sprintf(", %d of them unpriced", st.ThisMonth.UnpricedCalls)
	}
	if st.MonthlySpendCapUSD > 0 {
		month += ", of a " + dollars(st.MonthlySpendCapUSD) + " cap"
	}
	rows := [][]string{
		{"Enabled:", enabled},
		{"Provider:", st.Provider},
		{"Model:", st.Model},
		{"Endpoint:", st.Endpoint},
		{"API key:", key},
		{"Price:", price},
		{"This month:", month + " (UTC)"},
	}
	for _, r := range rows {
		fmt.Fprintf(a.stdout, "%-12s %s\n", r[0], r[1])
	}
	return nil
}

// priceText writes a price per million tokens.
func priceText(p intelligence.Price) string {
	s := fmt.Sprintf("$%s in, $%s out", amount(p.Input), amount(p.Output))
	if p.CacheRead > 0 {
		s += fmt.Sprintf(", $%s cache read", amount(p.CacheRead))
	}
	s += " per million tokens"
	if p.Note != "" {
		s += " (" + p.Note + ")"
	}
	if p.Configured {
		s += " (from config.yaml)"
	}
	return s
}

// amount writes a price without trailing zeros: 0.2, 4, 0.075.
func amount(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// intelligenceUsage is what tug intelligence usage shows.
type intelligenceUsage struct {
	Calls   []intelligence.Call  `json:"calls"`
	Daily   []intelligence.Total `json:"daily"`
	Monthly []intelligence.Total `json:"monthly"`
}

func newIntelligenceUsageCmd(a *app) *cobra.Command {
	var (
		output string
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Show the calls --deep made, and what they cost by day and month",
		Long: `Show the calls tug why --deep made, newest first, and their tokens and cost
by day and by month (UTC), from the ledger in tug's data directory. The ledger
keeps what each call took, never what was sent or answered, for a little over
a year. A call whose model has no price counts as unpriced, never as free.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			if format == render.FormatName {
				return errors.New("usage supports -o table, json or yaml")
			}
			calls, err := intelligence.OpenLedger(a.paths.Data, a.now).Calls()
			if err != nil {
				return err
			}
			u := intelligenceUsage{Calls: []intelligence.Call{}, Daily: intelligence.Daily(calls), Monthly: intelligence.Monthly(calls)}
			for i := len(calls) - 1; i >= 0 && (limit <= 0 || len(u.Calls) < limit); i-- {
				u.Calls = append(u.Calls, calls[i])
			}
			if u.Daily == nil {
				u.Daily, u.Monthly = []intelligence.Total{}, []intelligence.Total{}
			}
			if format != render.FormatTable {
				return render.Structured(a.stdout, format, u)
			}
			return a.printIntelligenceUsage(u)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json or yaml")
	cmd.Flags().IntVar(&limit, "limit", 20, "how many of the latest calls to list; 0 lists them all")
	return cmd
}

func (a *app) printIntelligenceUsage(u intelligenceUsage) error {
	w := a.stdout
	if len(u.Calls) == 0 {
		fmt.Fprintln(w, "No calls yet.")
		return nil
	}
	costOf := func(c *float64) string {
		if c == nil {
			return "unknown"
		}
		return dollars(*c)
	}
	rows := make([][]string, 0, len(u.Calls))
	for _, c := range u.Calls {
		rows = append(rows, []string{
			c.Time.Local().Format(time.DateTime), c.Provider, c.Model, cmp.Or(c.Target, "-"),
			tokenCount(c.InputTokens), tokenCount(c.OutputTokens), tokenCount(c.CacheReadTokens),
			costOf(c.CostUSD), fmt.Sprintf("%.1fs", float64(c.LatencyMS)/1000), cmp.Or(c.RequestID, "-"),
		})
	}
	if err := render.Table(w, []string{"TIME", "PROVIDER", "MODEL", "TARGET", "INPUT", "OUTPUT", "CACHED", "COST", "LATENCY", "REQUEST"}, rows); err != nil {
		return err
	}
	totals := func(header string, ts []intelligence.Total) error {
		rows := make([][]string, 0, len(ts))
		for _, t := range ts {
			cost := dollars(t.CostUSD)
			if t.UnpricedCalls > 0 {
				cost += fmt.Sprintf(" (+%d unpriced)", t.UnpricedCalls)
			}
			rows = append(rows, []string{t.Period, strconv.Itoa(t.Calls), tokenCount(t.InputTokens), tokenCount(t.OutputTokens), tokenCount(t.CacheReadTokens), cost})
		}
		fmt.Fprintln(w)
		return render.Table(w, []string{header, "CALLS", "INPUT", "OUTPUT", "CACHED", "COST"}, rows)
	}
	if err := totals("DAY (UTC)", u.Daily); err != nil {
		return err
	}
	return totals("MONTH (UTC)", u.Monthly)
}

func newIntelligenceKeyCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Store or remove a model provider's API key",
		Long: `Store a model provider's API key in the credential store, or remove it. tug
asks for the key on the terminal; it never takes one from a flag, an argument
or a file. In scripts and CI, set TUG_SECRET_<PROVIDER>_API_KEY instead, such
as TUG_SECRET_OPENAI_API_KEY, which wins over a stored key.`,
	}
	providerArg := func(args []string) (string, error) {
		p := strings.ToLower(args[0])
		return p, intelligence.CheckProvider(p)
	}
	set := &cobra.Command{
		Use:     "set <provider>",
		Short:   "Ask for a provider's API key and store it",
		Example: "  tug intelligence key set anthropic\n  tug intelligence key set openai",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := providerArg(args)
			if err != nil {
				return err
			}
			store, err := a.credentialStore()
			if err != nil {
				return err
			}
			v, err := a.readSecret(fmt.Sprintf("%s API key", p))
			switch {
			case errors.Is(err, errNotInteractive):
				return fmt.Errorf("tug intelligence key set asks for the key on a terminal; in scripts, set %s instead", intelligence.KeyEnv(p))
			case err != nil:
				return err
			case v == "":
				return errors.New("no key was entered, so nothing was stored")
			}
			if err := store.Set(credentials.IntelligenceKey(p), v); err != nil {
				return fmt.Errorf("storing the %s API key in %s: %w", p, store.Name(), err)
			}
			fmt.Fprintf(a.stdout, "Stored the %s API key in %s.\n", p, store.Name())
			return nil
		},
	}
	rm := &cobra.Command{
		Use:     "rm <provider>",
		Aliases: []string{"remove"},
		Short:   "Remove a provider's stored API key",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := providerArg(args)
			if err != nil {
				return err
			}
			store, err := a.credentialStore()
			if err != nil {
				return err
			}
			name := credentials.IntelligenceKey(p)
			if _, err := store.Get(name); errors.Is(err, credentials.ErrNotFound) {
				fmt.Fprintf(a.stdout, "No %s API key was stored in %s.\n", p, store.Name())
				return nil
			}
			if err := store.Delete(name); err != nil {
				return fmt.Errorf("removing the %s API key from %s: %w", p, store.Name(), err)
			}
			fmt.Fprintf(a.stdout, "Removed the %s API key from %s.\n", p, store.Name())
			return nil
		},
	}
	cmd.AddCommand(set, rm)
	return cmd
}

// intelligenceModels is what tug intelligence models shows.
type intelligenceModels struct {
	// Defaults are each provider's default model; custom has none.
	Defaults map[string]string          `json:"defaults"`
	Prices   []intelligence.Price       `json:"prices"`
	Sources  []intelligence.PriceSource `json:"sources"`
}

func newIntelligenceModelsCmd(a *app) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "models",
		Short: "List each provider's default model and the prices tug assumes",
		Long: `List each provider's default model and the price tug assumes for each model,
in US dollars per million tokens: the built-in table, with where and when its
prices were read, and any prices config.yaml adds under intelligence.prices,
which come first. A model id takes the price of the longest entry that starts
it; a model none starts has an unknown cost.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(output)
			if err != nil {
				return err
			}
			s, err := intelligence.SettingsFrom(a.cfg.Intelligence, intelligence.Overrides{})
			if err != nil {
				return err
			}
			m := intelligenceModels{Defaults: map[string]string{}, Prices: s.Prices.All(), Sources: intelligence.PriceSources()}
			for _, p := range intelligence.Providers {
				m.Defaults[p] = intelligence.DefaultModel(p)
			}
			switch format {
			case render.FormatName:
				for _, p := range m.Prices {
					fmt.Fprintln(a.stdout, p.Model)
				}
				return nil
			case render.FormatTable:
				return a.printIntelligenceModels(m)
			default:
				return render.Structured(a.stdout, format, m)
			}
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "output format: table, json, yaml or name")
	return cmd
}

func (a *app) printIntelligenceModels(m intelligenceModels) error {
	w := a.stdout
	rows := make([][]string, 0, len(intelligence.Providers))
	for _, p := range intelligence.Providers {
		rows = append(rows, []string{p, cmp.Or(m.Defaults[p], "none: set intelligence.model"), intelligence.DefaultBaseURL(p)})
	}
	if err := render.Table(w, []string{"PROVIDER", "DEFAULT MODEL", "ADDRESS"}, rows); err != nil {
		return err
	}
	rows = rows[:0]
	for _, p := range m.Prices {
		provider := p.Provider
		if p.Configured {
			provider = "config.yaml"
		}
		cache := "-"
		if p.CacheRead > 0 {
			cache = "$" + amount(p.CacheRead)
		}
		rows = append(rows, []string{provider, p.Model, "$" + amount(p.Input), "$" + amount(p.Output), cache, p.Note})
	}
	fmt.Fprintln(w)
	if err := render.Table(w, []string{"PROVIDER", "MODEL", "INPUT", "OUTPUT", "CACHE READ", "NOTE"}, rows); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nPrices are US dollars per million tokens, read from:")
	for _, s := range m.Sources {
		fmt.Fprintf(w, "  %-9s %s (%s)\n", s.Provider, s.URL, s.Checked)
	}
	return nil
}

// intelligenceKey finds a provider's API key: in TUG_SECRET_<PROVIDER>_API_KEY,
// or in the credential store. It returns "" when there is none, and where
// the key came from.
func (a *app) intelligenceKey(provider string) (key, source string, err error) {
	env := intelligence.KeyEnv(provider)
	if v := os.Getenv(env); v != "" {
		return v, env, nil
	}
	store, err := a.credentialStore()
	if err != nil {
		if !intelligence.KeyRequired(provider) {
			return "", "", nil // a custom endpoint may need no key, so no store either
		}
		return "", "", err
	}
	v, err := store.Get(credentials.IntelligenceKey(provider))
	switch {
	case errors.Is(err, credentials.ErrNotFound):
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("reading the %s API key from %s: %w", provider, store.Name(), err)
	}
	return v, store.Name(), nil
}

// tokenCount writes a number of tokens with thousands separators: 12,345.
func tokenCount(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// dollars writes a cost in dollars, with four places under a dollar, where
// most calls cost.
func dollars(v float64) string {
	if v != 0 && v < 1 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// The settings tug intelligence enable and disable change. They are found
// when tug starts, so a renamed one fails every test.
var (
	settingEnabled  = mustSetting("intelligence.enabled")
	settingProvider = mustSetting("intelligence.provider")
	settingModel    = mustSetting("intelligence.model")
	settingBaseURL  = mustSetting("intelligence.baseURL")
)

func mustSetting(key string) config.Setting {
	s, ok := config.LookupSetting(key)
	if !ok {
		panic("cli: no setting " + key) // a programmer error, during initialisation
	}
	return s
}

func newIntelligenceEnableCmd(a *app) *cobra.Command {
	var model, baseURL string
	cmd := &cobra.Command{
		Use:   "enable [provider]",
		Short: "Turn tug why --deep on with a provider, and store its API key",
		Long: `Turn tug why --deep on, with anthropic, openai, mistral or custom (any
OpenAI-compatible API, such as Ollama). Without a provider, it keeps the one
config.yaml names, or anthropic. Choosing another provider goes back to its
own model and address unless --model and --base-url say otherwise.

On a terminal, when the provider needs an API key and none is stored, tug asks
for it and keeps it in the keychain; leave it empty to add it later with
tug intelligence key set. --deep still shows what it would send, and what it
could cost, and asks before sending anything.`,
		Example: `  tug intelligence enable                    # Claude, with its default model
  tug intelligence enable openai --model gpt-6.2-sol
  tug intelligence enable custom --base-url http://localhost:11434/v1 --model llama4:70b`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: intelligence.Providers,
		RunE: func(cmd *cobra.Command, args []string) error {
			cur := a.cfg.Intelligence
			provider := strings.ToLower(cmp.Or(strings.TrimSpace(cur.Provider), intelligence.DefaultProvider))
			if len(args) == 1 {
				provider = strings.ToLower(args[0])
			}
			if err := intelligence.CheckProvider(provider); err != nil {
				return err
			}
			switching := !strings.EqualFold(provider, cmp.Or(strings.TrimSpace(cur.Provider), intelligence.DefaultProvider))
			changes := []config.Change{
				{Setting: settingEnabled, Value: "true"},
				{Setting: settingProvider, Value: provider},
			}
			for _, c := range []struct {
				setting config.Setting
				value   string
			}{{settingModel, model}, {settingBaseURL, baseURL}} {
				switch {
				case c.value != "":
					changes = append(changes, config.Change{Setting: c.setting, Value: c.value})
				case switching:
					// The configured one was for the other provider.
					changes = append(changes, config.Change{Setting: c.setting, Unset: true})
				}
			}
			var s intelligence.Settings
			check := func(c *config.Config) error {
				if err := a.checkSettings(c); err != nil {
					return err
				}
				var err error
				s, err = intelligence.SettingsFrom(c.Intelligence, intelligence.Overrides{})
				return err
			}
			if _, err := config.EditSettings(a.paths.Config, changes, check); err != nil {
				return enableHint(err)
			}
			fmt.Fprintf(a.stdout, "tug why --deep is on, asking %s's %s at %s.\n", s.Provider, s.Model, intelligence.NewClient(s, "").Endpoint())
			if err := a.offerIntelligenceKey(s.Provider); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "Try it with `tug why <target> --deep`: it shows what it would send, and the cost, before asking.")
			return nil
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "the model to ask (default: the provider's)")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "the API's address, for a gateway or a custom endpoint (default: the provider's)")
	return cmd
}

// enableHint points an error about a missing address at enable's own flag;
// the one about a missing model already names --model.
func enableHint(err error) error {
	if strings.Contains(err.Error(), "needs intelligence.baseURL") {
		return fmt.Errorf("%w; pass --base-url", err)
	}
	return err
}

// offerIntelligenceKey says where the provider's API key comes from or, when
// there is none, asks for one on a terminal and stores it.
func (a *app) offerIntelligenceKey(provider string) error {
	if !intelligence.KeyRequired(provider) {
		return nil
	}
	key, source, err := a.intelligenceKey(provider)
	switch {
	case err != nil:
		return err
	case key != "" && strings.HasPrefix(source, "TUG_SECRET_"):
		fmt.Fprintf(a.stdout, "It uses the API key in %s.\n", source)
		return nil
	case key != "":
		fmt.Fprintf(a.stdout, "It uses the %s API key stored in %s.\n", provider, source)
		return nil
	}
	later := fmt.Sprintf("Store the %s API key with `tug intelligence key set %s`, or set %s, before using it.", provider, provider, intelligence.KeyEnv(provider))
	if !a.interactive() {
		fmt.Fprintln(a.stdout, later)
		return nil
	}
	v, err := a.readSecret(fmt.Sprintf("%s API key (leave empty to add it later)", provider))
	if err != nil {
		return err
	}
	if v == "" {
		fmt.Fprintln(a.stdout, later)
		return nil
	}
	store, err := a.credentialStore()
	if err != nil {
		return err
	}
	if err := store.Set(credentials.IntelligenceKey(provider), v); err != nil {
		return fmt.Errorf("storing the %s API key in %s: %w", provider, store.Name(), err)
	}
	fmt.Fprintf(a.stdout, "Stored the %s API key in %s.\n", provider, store.Name())
	return nil
}

func newIntelligenceDisableCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Turn tug why --deep off",
		Long: `Turn tug why --deep off. The other intelligence settings, the usage ledger and
any stored API keys are kept, so tug intelligence enable turns it back on as
it was; remove a key with tug intelligence key rm <provider>.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := config.EditSettings(a.paths.Config, []config.Change{{Setting: settingEnabled, Value: "false"}}, a.checkSettings); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "tug why --deep is off. Stored API keys are kept; remove one with `tug intelligence key rm <provider>`.")
			return nil
		},
	}
}
