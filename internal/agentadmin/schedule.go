package agentadmin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// Schedules (agent-administered Vornik design §17). The renderer emits
// broker.schedule; the registry's loader validates it (cron, zone, the
// hourly floor, the inputs against the input schema), and the renderer runs
// that same check over its own output, so the two cannot disagree.

// maxScheduleInputBytes bounds a schedule's fixed inputs.
const maxScheduleInputBytes = 2048

// renderSchedule turns the agent's schedule into broker.schedule's JSON flow
// mapping, which is valid YAML. "" for none.
func renderSchedule(in *ScheduleInput) (string, string) {
	if in == nil {
		return "", ""
	}
	if len(in.Inputs) > maxScheduleInputBytes {
		return "", fmt.Sprintf("the schedule's inputs are over %d bytes", maxScheduleInputBytes)
	}
	inputs := map[string]any{}
	if len(bytes.TrimSpace(in.Inputs)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(in.Inputs))
		dec.UseNumber()
		if err := dec.Decode(&inputs); err != nil {
			return "", "the schedule's inputs must be a JSON object"
		}
	}
	out, err := canonicalOf(map[string]any{"cron": strings.TrimSpace(in.Cron), "timezone": strings.TrimSpace(in.Timezone), "inputs": inputs})
	if err != nil {
		return "", "the schedule's inputs must be a JSON object"
	}
	return string(out), ""
}

// scheduleSignature is the schedule's part of the reach signature: the
// canonical JSON of cron, zone (an empty zone is UTC) and inputs. "" for
// none, so a workflow without a schedule keeps the hash it had before
// schedules existed.
func scheduleSignature(s *registry.BrokerSchedule) (string, error) {
	if s == nil {
		return "", nil
	}
	tz := strings.TrimSpace(s.Timezone)
	if tz == "" {
		tz = "UTC"
	}
	inputs := s.Inputs
	if inputs == nil {
		inputs = map[string]any{}
	}
	raw, err := canonicalOf(map[string]any{"cron": strings.TrimSpace(s.Cron), "timezone": tz, "inputs": inputs})
	return string(raw), err
}

// scheduleSentence is what the approval says about running unattended: the
// schedule in words, its zone and its inputs; or, when an approved schedule
// is dropped, that it stops.
func scheduleSentence(id string, s *registry.BrokerSchedule, prev *WorkflowState) string {
	if s == nil {
		if prev != nil && prev.Loaded != nil && prev.Loaded.Broker != nil && prev.Loaded.Broker.Schedule != nil {
			return " It will no longer run automatically."
		}
		return ""
	}
	tz := strings.TrimSpace(s.Timezone)
	if tz == "" {
		tz = "UTC"
	}
	inputs, _ := canonicalOf(s.Inputs)
	return fmt.Sprintf(" It will also run %q automatically, %s (%s), with %s.", id, describeCron(s.Cron), tz, inputs)
}

var monthNames = []string{"", "January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}

var dayNames = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}

// describeCron says a 5-field cron in plain words, for the forms it can say
// exactly: a single value, a comma list or "*" in each field. Anything else
// (steps, ranges, names, day-of-month together with day-of-week, which cron
// ORs) is shown as the expression itself: the sentence never guesses (review
// 20261002-6a3f R6).
func describeCron(expr string) string {
	fallback := "on the cron schedule `" + strings.TrimSpace(expr) + "`"
	f := strings.Fields(expr)
	if len(f) != 5 {
		return fallback
	}
	minute, mok := values(f[0], 0, 59)
	hour, hok := values(f[1], 0, 23)
	dom, dok := values(f[2], 1, 31)
	month, monok := values(f[3], 1, 12)
	dow, wok := values(f[4], 0, 7)
	if !mok || !hok || !dok || !monok || !wok || len(minute) != 1 || (dom != nil && dow != nil) {
		return fallback
	}
	var when string
	switch {
	case hour == nil && dom == nil && month == nil && dow == nil:
		return fmt.Sprintf("at minute %d of every hour", minute[0])
	case len(hour) == 1:
		when = fmt.Sprintf("at %02d:%02d", hour[0], minute[0])
	default:
		return fallback
	}
	switch {
	case dom != nil && month == nil:
		return when + " on " + dayWord(dom) + " of every month"
	case dom != nil:
		return when + " on " + dayWord(dom) + " of " + names(month, monthNames)
	case dow != nil && month == nil:
		return when + " on " + names(dow, dayNames)
	case dow != nil:
		return when + " on " + names(dow, dayNames) + " in " + names(month, monthNames)
	case month != nil:
		return when + " every day in " + names(month, monthNames)
	}
	return when + " every day"
}

// values parses "*" (nil, ok) or a comma list of numbers in [lo, hi].
func values(field string, lo, hi int) ([]int, bool) {
	if field == "*" {
		return nil, true
	}
	var out []int
	for _, part := range strings.Split(field, ",") {
		n, err := strconv.Atoi(part)
		if err != nil || n < lo || n > hi {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func dayWord(days []int) string {
	if len(days) == 1 {
		return "day " + strconv.Itoa(days[0])
	}
	parts := make([]string, len(days))
	for i, d := range days {
		parts[i] = strconv.Itoa(d)
	}
	return "days " + joinWords(parts)
}

func names(vals []int, table []string) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = table[v]
	}
	return joinWords(parts)
}

// joinWords is "a", "a and b", "a, b and c".
func joinWords(parts []string) string {
	if len(parts) <= 1 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// DescribeSchedule is a schedule in words with its zone, as the approval
// sentence and list_my_setup say it.
func DescribeSchedule(s *registry.BrokerSchedule) string {
	if s == nil {
		return ""
	}
	tz := strings.TrimSpace(s.Timezone)
	if tz == "" {
		tz = "UTC"
	}
	return describeCron(s.Cron) + " (" + tz + ")"
}
