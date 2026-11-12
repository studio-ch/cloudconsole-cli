package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/studio-ch/cloudconsole-cli/internal/api"
	"github.com/studio-ch/cloudconsole-cli/internal/output"
	"github.com/studio-ch/cloudconsole-cli/internal/wait"
)

func snapshotColumns(now time.Time) []output.Column {
	return []output.Column{
		{Header: "id", Value: func(r map[string]any) string { return output.Field(r, "id") }},
		{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
		{Header: "status", Value: func(r map[string]any) string { return output.Field(r, "status") }},
		{Header: "instance", Value: snapshotSourceCell},
		{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
		{Header: "disk", Value: func(r map[string]any) string { return gib(r, "diskGib") }},
		{Header: "age", Value: func(r map[string]any) string { return output.Age(output.Field(r, "createdAt"), now) }},

		{Header: "platform", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "platform") }},
		{Header: "min cpu", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "minCpuCores") }},
		{Header: "min memory", Wide: true, Value: func(r map[string]any) string { return gib(r, "minMemoryGib") }},
		{Header: "while running", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "takenWhileRunning") }},
		{Header: "captured", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "captureTime") }},
		{Header: "restorable", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "restorable") }},
		{Header: "description", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "description") }},
		{Header: "error", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "lastError") }},
	}
}

// snapshotSourceCell names the instance a snapshot was taken of. A
// snapshot outlives its instance, and whether it still can restore it is
// what decides between 'instance restore' and 'instance create
// --from-snapshot', so a deleted source is called out rather than shown
// as a bare name that no longer resolves.
func snapshotSourceCell(r map[string]any) string {
	name := output.Field(r, "sourceInstanceName")
	if output.Field(r, "sourceInstanceId") == "" {
		return name + " (deleted)"
	}
	return name
}

func newSnapshotCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "snapshot",
		Aliases: []string{"snapshots", "snap"},
		Short:   "Manage Xcloud instance snapshots",
		Long: "A snapshot holds an instance's boot disk and firmware settings — never its\n" +
			"memory or attached volumes. Taken from a running instance it is\n" +
			"crash-consistent, as after a sudden power loss.\n\n" +
			"A snapshot restores only the instance it was taken of\n" +
			"('cloudconsole instance restore'), seeds new instances\n" +
			"('cloudconsole instance create --from-snapshot'), and outlives its instance.\n\n" +
			"Taking and deleting a snapshot are asynchronous; pass --wait to block until\n" +
			"the snapshot is ready, or gone. Snapshots can be referred to by id or name.",
	}
	c.AddCommand(
		newSnapshotListCommand(s),
		newSnapshotGetCommand(s),
		newSnapshotCreateCommand(s),
		newSnapshotUpdateCommand(s),
		newSnapshotDeleteCommand(s),
	)
	return c
}

func newSnapshotListCommand(s *State) *cobra.Command {
	var instance, region string
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List snapshots, including those of deleted instances",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			query := url.Values{}
			if instance != "" {
				query.Set("instanceId", instance)
			}
			if region != "" {
				regionID, err := resolveRegion(cmd.Context(), client, region)
				if err != nil {
					return err
				}
				query.Set("regionId", regionID)
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodGet, Path: "/v1/xcloud/snapshots", Query: query,
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, snapshotColumns(time.Now()))
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "only list snapshots of this instance id")
	c.Flags().StringVar(&region, "region", "", "only list snapshots in this region (slug or id)")
	return c
}

func newSnapshotGetCommand(s *State) *cobra.Command {
	return &cobra.Command{
		Use:   "get <id|name>",
		Short: "Show one snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			id, err := resolveSnapshotID(cmd.Context(), client, args[0], "")
			if err != nil {
				return err
			}
			return renderGet(cmd, s, "/v1/xcloud/snapshots/"+id, snapshotColumns)
		},
	}
}

func newSnapshotCreateCommand(s *State) *cobra.Command {
	var (
		instance    string
		name        string
		description string
		wf          waitFlags
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Take a snapshot of an instance",
		Long: "Take a snapshot of an instance's boot disk and firmware settings.\n\n" +
			"A running instance is snapshotted in place and the result is\n" +
			"crash-consistent; stop or shut it down first for a clean one. A suspended\n" +
			"instance is refused. Without --name the snapshot is named\n" +
			"'<instance name> <UTC timestamp>'.\n\n" +
			"The snapshot is created 'creating' and turns 'ready' or 'failed'\n" +
			"asynchronously; --wait blocks until then and fails on 'failed'.",
		Example: "  cloudconsole snapshot create --instance <instance-id> --name before-upgrade --wait",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			body := map[string]any{}
			if name != "" {
				body["name"] = name
			}
			if description != "" {
				body["description"] = description
			}
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost,
				Path:   "/v1/xcloud/instances/" + instance + "/snapshots",
				Body:   payload,
			})
			if err != nil {
				return err
			}

			var created struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(resp.Body, &created)
			if wf.enabled && created.ID != "" {
				if err := awaitSnapshot(cmd, s, created.ID, wait.SnapshotReadyOrFailed(), wf, "taking snapshot"); err != nil {
					return err
				}
				fresh, ferr := client.Do(cmd.Context(), api.RequestOptions{
					Method: http.MethodGet, Path: "/v1/xcloud/snapshots/" + created.ID,
				})
				if ferr == nil {
					return w.Render(fresh.Body, snapshotColumns(time.Now()))
				}
			}
			return w.Render(resp.Body, snapshotColumns(time.Now()))
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "instance id to snapshot (required)")
	c.Flags().StringVar(&name, "name", "", "snapshot name, unique in your organisation (default '<instance> <timestamp>')")
	c.Flags().StringVar(&description, "description", "", "free-text description")
	_ = c.MarkFlagRequired("instance")
	wf.register(c)
	return c
}

func newSnapshotUpdateCommand(s *State) *cobra.Command {
	var name, description string
	c := &cobra.Command{
		Use:     "update <id|name>",
		Aliases: []string{"rename"},
		Short:   "Rename or re-describe a snapshot",
		Long: "Change a snapshot's name or description. Pass --description \"\" to clear\n" +
			"the description. This is metadata only; the snapshot itself is untouched.",
		Example: "  cloudconsole snapshot rename before-upgrade --name macos-15.4-baseline",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nameSet := cmd.Flags().Changed("name")
			descSet := cmd.Flags().Changed("description")
			if !nameSet && !descSet {
				return &usageError{fmt.Errorf("specify --name, --description or both")}
			}
			if nameSet && name == "" {
				return &usageError{fmt.Errorf("--name must not be empty")}
			}
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			id, err := resolveSnapshotID(cmd.Context(), client, args[0], "")
			if err != nil {
				return err
			}
			body := map[string]any{}
			if nameSet {
				body["name"] = name
			}
			if descSet {
				// An empty description clears it; the API models that as
				// an explicit null.
				if description == "" {
					body["description"] = nil
				} else {
					body["description"] = description
				}
			}
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPatch, Path: "/v1/xcloud/snapshots/" + id, Body: payload,
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, snapshotColumns(time.Now()))
		},
	}
	c.Flags().StringVar(&name, "name", "", "new name")
	c.Flags().StringVar(&description, "description", "", "new description (empty clears it)")
	return c
}

func newSnapshotDeleteCommand(s *State) *cobra.Command {
	var (
		yes bool
		wf  waitFlags
	)
	c := &cobra.Command{
		Use:     "delete <id|name>",
		Aliases: []string{"rm"},
		Short:   "Delete a snapshot",
		Long: "Delete a snapshot. Instances created from it keep running.\n\n" +
			"Deletion is asynchronous: the snapshot reads 'deleting' until xcloud has\n" +
			"reclaimed its storage, which waits while an instance created from it has\n" +
			"never started. --wait blocks until it is gone.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			id, err := resolveSnapshotID(cmd.Context(), client, args[0], "")
			if err != nil {
				return err
			}
			if !yes {
				ok, err := s.Confirm(fmt.Sprintf(
					"Delete snapshot %s? This cannot be undone. [y/N]: ", args[0]))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(w.Err, "Aborted.")
					return nil
				}
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodDelete, Path: "/v1/xcloud/snapshots/" + id,
			}); err != nil {
				return err
			}
			if !wf.enabled {
				if !w.Quiet {
					fmt.Fprintf(w.Err, "Delete queued for snapshot %s. Add --wait to block until it finishes.\n", args[0])
				}
				return nil
			}
			return awaitSnapshot(cmd, s, id, wait.SnapshotGone(), wf, "deleting snapshot")
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	wf.register(c)
	return c
}

// snapshotRecord is the subset of a snapshot the instance commands need.
type snapshotRecord struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	RegionID     string `json:"regionId"`
	Status       string `json:"status"`
	Platform     string `json:"platform"`
	DiskGib      int    `json:"diskGib"`
	MinCPUCores  *int   `json:"minCpuCores"`
	MinMemoryGib *int   `json:"minMemoryGib"`
}

// resolveSnapshotID turns a snapshot id or name into its id. A UUID is
// passed through untouched, without a request; anything else is looked up
// by exact name, which is unique per organisation. instanceID narrows the
// lookup to one instance's snapshots — restore can only use those anyway.
func resolveSnapshotID(ctx context.Context, c *api.Client, value, instanceID string) (string, error) {
	if looksLikeUUID(value) {
		return value, nil
	}
	rec, err := findSnapshotByName(ctx, c, value, instanceID)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// lookupSnapshot returns the full record for an id or name.
func lookupSnapshot(ctx context.Context, c *api.Client, value string) (snapshotRecord, error) {
	if !looksLikeUUID(value) {
		return findSnapshotByName(ctx, c, value, "")
	}
	resp, err := c.Do(ctx, api.RequestOptions{Method: http.MethodGet, Path: "/v1/xcloud/snapshots/" + value})
	if err != nil {
		return snapshotRecord{}, err
	}
	var rec snapshotRecord
	if err := json.Unmarshal(resp.Body, &rec); err != nil {
		return snapshotRecord{}, fmt.Errorf("could not read the snapshot: %w", err)
	}
	return rec, nil
}

func findSnapshotByName(ctx context.Context, c *api.Client, name, instanceID string) (snapshotRecord, error) {
	query := url.Values{}
	if instanceID != "" {
		query.Set("instanceId", instanceID)
	}
	resp, err := c.Do(ctx, api.RequestOptions{Method: http.MethodGet, Path: "/v1/xcloud/snapshots", Query: query})
	if err != nil {
		return snapshotRecord{}, err
	}
	var envelope struct {
		Data []snapshotRecord `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return snapshotRecord{}, fmt.Errorf("could not read the snapshot list: %w", err)
	}
	var matches []snapshotRecord
	for _, r := range envelope.Data {
		if r.Name == name {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		where := ""
		if instanceID != "" {
			where = " of instance " + instanceID
		}
		return snapshotRecord{}, &usageError{fmt.Errorf(
			"no snapshot named %q%s; 'cloudconsole snapshot list' shows the ones available", name, where)}
	default:
		return snapshotRecord{}, &usageError{fmt.Errorf(
			"%d snapshots are named %q; refer to one by id", len(matches), name)}
	}
}

// fetchSnapshotState reads one snapshot for the waiter, translating a 404
// into "does not exist" — a delete completes when the row stops existing.
func fetchSnapshotState(ctx context.Context, c *api.Client, id string) (wait.State, error) {
	resp, err := c.Do(ctx, api.RequestOptions{Method: http.MethodGet, Path: "/v1/xcloud/snapshots/" + id})
	if err != nil {
		var p *api.Problem
		if errors.As(err, &p) && (p.Status == http.StatusNotFound || p.Status == http.StatusGone) {
			return wait.State{Exists: false}, nil
		}
		return wait.State{}, err
	}
	var st struct {
		Status    string  `json:"status"`
		LastError *string `json:"lastError"`
	}
	if err := json.Unmarshal(resp.Body, &st); err != nil {
		return wait.State{}, fmt.Errorf("could not read the snapshot state: %w", err)
	}
	return wait.State{Exists: true, Status: st.Status, LastError: deref(st.LastError)}, nil
}

// awaitSnapshot is awaitInstance for snapshots: progress on stderr, the
// same timeout and failure semantics.
func awaitSnapshot(cmd *cobra.Command, s *State, id string, predicate wait.Predicate, flags waitFlags, verb string) error {
	client, err := s.Client()
	if err != nil {
		return err
	}
	return runWait(cmd, s, flags, verb,
		waitTimeoutError{Resource: "snapshot " + id, Check: "cloudconsole snapshot get " + id},
		func(ctx context.Context) (wait.State, error) { return fetchSnapshotState(ctx, client, id) },
		predicate)
}
