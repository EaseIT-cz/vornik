package datasubject

// Art 15(1)(a), (c) and (d): the purposes, the recipients and the retention
// windows the access report states (GitHub #470, GDPR design §4.22).
//
// The purposes are a Go mirror of the controller's record (docs/legal/ropa.md
// §2), kept equal to it by TestROPAActivitiesMirrorTheRecord. Recipients and
// retention depend on configuration, which this package cannot import
// (config imports datasubject), so the CLI resolves them and hands them in
// through SetRecipients and SetRetention. Until it does, the report says
// "unknown" rather than leaving a field empty: an empty list would read as
// "none".

// ROPASource names where the purposes come from.
const ROPASource = "docs/legal/ropa.md §2"

// Unknown is how the report states a value it cannot know. Every unknown is
// followed by its reason.
const Unknown = "unknown"

// Recipient locations.
const (
	LocationLocal  = "local"
	LocationRemote = "remote"
)

// Recipient categories.
const (
	CategoryModelProvider = "model provider"
	CategoryToolServer    = "tool server"
	CategoryLogSink       = "log sink"
)

// Where a retention window's value came from.
const (
	WindowConfigured = "configured"
	WindowDefault    = "default"
	WindowNotSet     = "not_set"
	WindowNone       = "none"
)

// ProcessingActivity is one row of the ROPA's processing activities.
type ProcessingActivity struct {
	ID       string `json:"id"`
	Activity string `json:"activity"`
	Purpose  string `json:"purpose"`
	Source   string `json:"source"`
}

// ROPAActivities mirrors docs/legal/ropa.md §2. The Art 6 basis is not
// mirrored: the ROPA marks it [TEMPLATE — REPLACE], so a mirrored value would
// be the worked example rather than the controller's answer.
var ROPAActivities = []ProcessingActivity{
	{ID: "P1", Activity: "Conversational task intake", Purpose: "Operate the agent platform"},
	{ID: "P2", Activity: "Long-term project memory (RAG)", Purpose: "Continuity across sessions"},
	{ID: "P3", Activity: "Audit + accountability logging", Purpose: "Security, debugging, Art 5(2) accountability, EU AI Act Art 50/99 evidence"},
	{ID: "P4", Activity: "Inbound email processing", Purpose: "Task intake by mail"},
	{ID: "P5", Activity: "Code-forge integration", Purpose: "Automated development work"},
	{ID: "P6", Activity: "Operator identity + access control", Purpose: "Authentication, authorisation"},
	{ID: "P7", Activity: "Media perception (image / audio / video analysis)", Purpose: "Answer an operator's question about a file they sent"},
	{ID: "P8", Activity: "Data-subject rights handling", Purpose: "Record, verify and answer requests under Arts 15–21, track the Art 12(3) deadline, and keep evidence of the response"},
}

// Recipient is one recipient, or category of recipient, of personal data.
type Recipient struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Receives string `json:"receives"`
	// Destination is the endpoint host, or "unknown: <reason>".
	Destination string `json:"destination"`
	// Location is local, remote or unknown.
	Location string `json:"location"`
	// Region is set only where the configuration names one.
	Region    string `json:"region,omitempty"`
	ConfigKey string `json:"config_key,omitempty"`
}

// StaticRecipients are the ROPA §5 categories configuration does not list:
// each project configures its own MCP servers, and log shipping is outside
// Vornik's configuration.
func StaticRecipients() []Recipient {
	return []Recipient{
		{Name: "MCP servers configured for a project", Category: CategoryToolServer,
			Receives:    "tool-call arguments",
			Destination: Unknown + ": each project configures its own tool servers; this report does not list them",
			Location:    Unknown},
		{Name: "the operator's log sinks", Category: CategoryLogSink,
			Receives:    "application and audit logs",
			Destination: Unknown + ": log shipping is configured outside Vornik",
			Location:    Unknown},
	}
}

// noProviderRead is the model-provider entry of a report built without
// configuration.
var noProviderRead = Recipient{Name: "model providers", Category: CategoryModelProvider,
	Receives:    "prompt content",
	Destination: Unknown + ": the configuration was not read for this report",
	Location:    Unknown}

// noProviderConfigured is the model-provider entry when the configuration
// enables none.
var noProviderConfigured = Recipient{Name: "model providers", Category: CategoryModelProvider,
	Receives:    "prompt content",
	Destination: "none: the configuration enables no model provider",
	Location:    "none"}

// RetentionWindow is how long one class of data is kept.
type RetentionWindow struct {
	Data      string   `json:"data"`
	Tables    []string `json:"tables,omitempty"`
	ConfigKey string   `json:"config_key,omitempty"`
	// Days is the effective window; zero when there is none.
	Days int `json:"days,omitempty"`
	// Source is configured, default, not_set or none.
	Source string `json:"source"`
	// Period is the sentence the subject reads.
	Period string `json:"period"`
}

// RetentionStatement is the report's Art 15(1)(d) section.
type RetentionStatement struct {
	// Known is false when the configuration was not read.
	Known          bool              `json:"known"`
	SweeperEnabled bool              `json:"sweeper_enabled"`
	Scope          string            `json:"scope"`
	Windows        []RetentionWindow `json:"windows"`
}

// unknownRetention is the statement of a report built without configuration.
func unknownRetention() RetentionStatement {
	return RetentionStatement{
		Scope:   Unknown + ": the configuration was not read for this report",
		Windows: []RetentionWindow{},
	}
}

// SetRecipients records the model providers the configuration enables,
// followed by the static ROPA §5 categories. None enabled is stated.
func (e *Export) SetRecipients(providers []Recipient) {
	if len(providers) == 0 {
		providers = []Recipient{noProviderConfigured}
	}
	e.Recipients = append(append([]Recipient(nil), providers...), StaticRecipients()...)
}

// SetRetention records the retention statement resolved from configuration.
func (e *Export) SetRetention(st RetentionStatement) {
	if st.Windows == nil {
		st.Windows = []RetentionWindow{}
	}
	e.Retention = st
}

func ropaPurposes() []ProcessingActivity {
	out := make([]ProcessingActivity, len(ROPAActivities))
	for i, a := range ROPAActivities {
		a.Source = ROPASource
		out[i] = a
	}
	return out
}
