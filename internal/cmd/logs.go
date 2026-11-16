package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teraldev/cli/internal/api"
	"github.com/teraldev/cli/internal/client"
)

type logsOptions struct {
	deployment string
	build      bool
	follow     bool
	since      string
	until      string
	levels     []string
	grep       string
	lines      int
	timestamps bool
}

func newLogsCmd(a *App) *cobra.Command {
	var o logsOptions
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show runtime or build logs",
		Long: `Print an app's runtime logs (what its instances write to stdout and stderr), or
with --build the build output of a deployment. -f keeps following new lines.

Without --deployment, runtime logs cover every deployment of the environment
and build logs show the newest deployment.`,
		Example: `  teral logs
  teral logs -f
  teral logs --since 2h --level error,warn --grep timeout
  teral logs --build -f
  teral logs -d d_1234abcd`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			c, _, err := a.Client()
			if err != nil {
				return err
			}
			if o.build {
				return a.buildLogs(ctx, c, o)
			}
			return a.runtimeLogs(ctx, c, o)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.deployment, "deployment", "d", "", "Only this deployment")
	f.BoolVar(&o.build, "build", false, "Show build output instead of runtime logs")
	f.BoolVarP(&o.follow, "follow", "f", false, "Keep printing new lines")
	f.StringVar(&o.since, "since", "", "Start time: a duration like 30m or 2h, or an RFC 3339 time (default 1h)")
	f.StringVar(&o.until, "until", "", "End time: a duration ago or an RFC 3339 time")
	f.StringSliceVarP(&o.levels, "level", "l", nil, "Only these severities: debug, info, warn, error, fatal")
	f.StringVarP(&o.grep, "grep", "g", "", "Only lines containing this text (case-insensitive)")
	f.IntVarP(&o.lines, "lines", "n", 100, "Number of lines to show before following")
	f.BoolVar(&o.timestamps, "date", false, "Print full dates instead of times")
	return cmd
}

// parseTime accepts "30m"-style durations (ago) and RFC 3339 times.
func parseTime(v string, now time.Time) (int64, error) {
	if v == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(-d).UnixMilli(), nil
	}
	if strings.HasSuffix(v, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil {
			return now.Add(-time.Duration(n) * 24 * time.Hour).UnixMilli(), nil
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, &UserError{Message: fmt.Sprintf("Can't read the time %q.", v), Hint: "Use a duration like 30m, 2h, or 3d, or a time like 2026-11-16T09:00."}
}

func (a *App) runtimeLogs(ctx context.Context, c *client.Client, o logsOptions) error {
	t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
	if err != nil {
		return err
	}
	now := time.Now()
	since, err := parseTime(o.since, now)
	if err != nil {
		return err
	}
	until, err := parseTime(o.until, now)
	if err != nil {
		return err
	}
	var severities []api.V1LogsQueryRuntimeLogsRequestBodySeverity
	for _, l := range o.levels {
		for _, part := range strings.Split(l, ",") {
			if part = strings.ToUpper(strings.TrimSpace(part)); part != "" {
				if part == "WARNING" {
					part = "WARN"
				}
				severities = append(severities, api.V1LogsQueryRuntimeLogsRequestBodySeverity(part))
			}
		}
	}
	base := api.V1LogsQueryRuntimeLogsRequestBody{
		Project:     t.Project,
		App:         &t.App,
		Environment: &t.Environment,
	}
	if o.deployment != "" {
		base.DeploymentId = &o.deployment
	}
	if len(severities) > 0 {
		base.Severity = &severities
	}
	if o.grep != "" {
		base.Query = &o.grep
	}

	query := func(req api.V1LogsQueryRuntimeLogsRequestBody) (*api.V1LogsQueryRuntimeLogsResponseBody, error) {
		return client.Call[api.V1LogsQueryRuntimeLogsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
			return cl.LogsQueryRuntimeLogs(ctx, req)
		})
	}

	// History: newest first from the API, printed oldest first.
	limit := min(max(o.lines, 1), 1000)
	history := base
	history.Limit = &limit
	if since > 0 {
		history.Since = &since
	}
	if until > 0 {
		history.Until = &until
	}
	seen := newSeenSet(5000)
	var lastTime int64
	if o.lines > 0 || !o.follow {
		var logs []api.RuntimeLog
		var cursor *string
		for len(logs) < o.lines {
			req := history
			req.Cursor = cursor
			res, err := query(req)
			if err != nil {
				return err
			}
			logs = append(logs, res.Data...)
			if !res.Pagination.HasMore || res.Pagination.Cursor == nil {
				break
			}
			cursor = res.Pagination.Cursor
		}
		if len(logs) > o.lines {
			logs = logs[:o.lines]
		}
		sort.SliceStable(logs, func(i, j int) bool { return logs[i].Time < logs[j].Time })
		for _, l := range logs {
			seen.add(l.Id)
			a.printRuntimeLog(l, o)
			lastTime = max(lastTime, l.Time)
		}
		if len(logs) == 0 && !o.follow && !a.IO.JSON {
			a.IO.Info("No logs in this time range. Widen it with --since, e.g. --since 24h.")
		}
	}
	if !o.follow {
		return nil
	}

	// Follow: an ascending cursor from the last line seen. The API never
	// repeats a row through the cursor, but lines that reach the log store
	// late (with an earlier timestamp) are only found by re-querying a short
	// window now and then, de-duplicated by id.
	if lastTime == 0 {
		lastTime = now.Add(-5 * time.Second).UnixMilli()
	}
	asc := api.V1LogsQueryRuntimeLogsRequestBodyOrder("asc")
	follow := base
	follow.Order = &asc
	follow.Limit = ptr(500)
	start := lastTime
	follow.Since = &start
	var cursor *string
	lastSweep := time.Now()
	failures := 0

	if !a.IO.JSON {
		fmt.Fprintln(a.IO.Err, a.IO.ErrTheme.Muted.Render("Following logs. Press Ctrl+C to stop."))
	}
	for {
		req := follow
		req.Cursor = cursor
		res, err := query(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failures++
			if failures >= 5 {
				return err
			}
		} else {
			failures = 0
			for _, l := range res.Data {
				if seen.add(l.Id) {
					a.printRuntimeLog(l, o)
					lastTime = max(lastTime, l.Time)
				}
			}
			if res.Pagination.Cursor != nil {
				cursor = res.Pagination.Cursor
			}
			if res.Pagination.HasMore {
				continue
			}
		}

		if time.Since(lastSweep) > 10*time.Second {
			lastSweep = time.Now()
			sweep := base
			sweep.Order = &asc
			sweep.Limit = ptr(500)
			from := lastTime - 15_000
			sweep.Since = &from
			if res, err := query(sweep); err == nil {
				late := res.Data[:0]
				for _, l := range res.Data {
					if seen.add(l.Id) {
						late = append(late, l)
					}
				}
				for _, l := range late {
					a.printRuntimeLog(l, o)
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func (a *App) printRuntimeLog(l api.RuntimeLog, o logsOptions) {
	if a.IO.JSON {
		raw, _ := json.Marshal(l)
		a.IO.Println(string(raw))
		return
	}
	t := a.IO.Theme
	ts := time.UnixMilli(l.Time)
	stamp := ts.Format("15:04:05.000")
	if o.timestamps {
		stamp = ts.Format("2006-01-02 15:04:05.000")
	}
	sev := strings.ToUpper(l.Severity)
	sevStyle := t.Muted
	switch sev {
	case "ERROR", "FATAL":
		sevStyle = t.Danger
	case "WARN", "WARNING":
		sevStyle = t.Warning
	case "INFO":
		sevStyle = t.Accent
	}
	source := l.DeploymentId
	if l.InstanceId != "" && l.InstanceId != "—" {
		source = l.InstanceId
	}
	a.IO.Printf("%s %s %s %s\n", t.Muted.Render(stamp), sevStyle.Render(fmt.Sprintf("%-5s", sev)), t.Muted.Render(source), strings.TrimRight(l.Message, "\n"))
}

func (a *App) buildLogs(ctx context.Context, c *client.Client, o logsOptions) error {
	id := o.deployment
	if id == "" {
		t, err := a.ResolveTarget(ctx, c, NeedEnvironment)
		if err != nil {
			return err
		}
		latest, err := latestDeployment(ctx, c, t, false)
		if err != nil {
			return err
		}
		id = latest.Id
		if !a.IO.JSON {
			fmt.Fprintf(a.IO.Err, "%s\n", a.IO.ErrTheme.Muted.Render("Build of "+id+" ("+string(latest.Status)+")"))
		}
	}

	var afterSeq int64
	seenSteps := map[string]bool{}
	trailing := 0
	for {
		req := api.V1LogsGetBuildLogsRequestBody{DeploymentId: id}
		if afterSeq > 0 {
			req.AfterSeq = &afterSeq
		}
		res, err := client.Call[api.V1LogsGetBuildLogsResponseBody](ctx, c, func(ctx context.Context, cl *api.Client) (*http.Response, error) {
			return cl.LogsGetBuildLogs(ctx, req)
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		d := res.Data
		a.printBuildPage(d, seenSteps)
		afterSeq = d.NextSeq
		if d.HasMore {
			continue
		}
		building := d.Status == api.DeploymentStatusBuilding || d.Status == api.DeploymentStatusPending || d.Status == api.DeploymentStatusStarting
		if !o.follow {
			return nil
		}
		if !building {
			// Lines are buffered before they reach the log store; fetch
			// twice more after the build ends so the tail isn't lost.
			trailing++
			if trailing > 2 {
				if !a.IO.JSON {
					fmt.Fprintf(a.IO.Err, "%s\n", a.IO.Theme.Status(string(d.Status)))
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}

func (a *App) printBuildPage(d api.V1LogsGetBuildLogsResponseData, seenSteps map[string]bool) {
	if a.IO.JSON {
		for _, l := range d.Lines {
			raw, _ := json.Marshal(l)
			a.IO.Println(string(raw))
		}
		return
	}
	t := a.IO.Theme
	names := map[string]string{}
	for _, s := range d.Steps {
		names[s.Id] = s.Name
	}
	printStep := func(id string) {
		if id == "" || seenSteps[id] {
			return
		}
		seenSteps[id] = true
		for _, s := range d.Steps {
			if s.Id != id {
				continue
			}
			suffix := ""
			if s.Cached {
				suffix = t.Muted.Render(" cached")
			}
			if s.Error != "" {
				suffix = t.Danger.Render(" failed")
			}
			a.IO.Printf("%s %s%s\n", t.Accent.Render("▸"), s.Name, suffix)
		}
	}
	for _, l := range d.Lines {
		printStep(l.StepId)
		style := t.Muted
		if l.Stderr {
			style = t.Warning
		}
		for _, part := range strings.Split(strings.TrimRight(l.Message, "\n"), "\n") {
			a.IO.Printf("  %s\n", style.Render(part))
		}
	}
	for _, s := range d.Steps {
		printStep(s.Id)
	}
}

// seenSet remembers recent log ids to drop duplicates between polls.
type seenSet struct {
	order []string
	ids   map[string]struct{}
	limit int
}

func newSeenSet(limit int) *seenSet {
	return &seenSet{ids: map[string]struct{}{}, limit: limit}
}

// add records id and reports whether it was new.
func (s *seenSet) add(id string) bool {
	if _, ok := s.ids[id]; ok {
		return false
	}
	s.ids[id] = struct{}{}
	s.order = append(s.order, id)
	if len(s.order) > s.limit {
		delete(s.ids, s.order[0])
		s.order = s.order[1:]
	}
	return true
}
