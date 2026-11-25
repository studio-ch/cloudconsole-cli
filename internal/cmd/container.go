package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/studio-ch/cloudconsole-cli/internal/api"
	"github.com/studio-ch/cloudconsole-cli/internal/output"
	"github.com/studio-ch/cloudconsole-cli/internal/wait"
)

const containersPath = "/v1/xcloud/containers"

func containerColumns(now time.Time) []output.Column {
	return []output.Column{
		{Header: "id", Value: func(r map[string]any) string { return output.Field(r, "id") }},
		{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
		{Header: "status", Value: containerStatusCell},
		{Header: "image", Value: func(r map[string]any) string { return output.Field(r, "image") }},
		{Header: "size", Value: containerSizeCell},
		{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
		{Header: "address", Value: func(r map[string]any) string { return output.Field(r, "networkAddress") }},
		{Header: "age", Value: func(r map[string]any) string { return output.Age(output.Field(r, "createdAt"), now) }},
	}
}

// containerStatusCell shows the action under way rather than the state the
// container is leaving, and the exit code of one that has ended.
func containerStatusCell(r map[string]any) string {
	if action := output.Field(r, "pendingAction"); action != "" {
		return action
	}
	status := output.Field(r, "status")
	if status == wait.ContainerExited {
		if code := output.Field(r, "exitCode"); code != "" {
			return "exited (" + code + ")"
		}
	}
	return status
}

func containerSizeCell(r map[string]any) string {
	cpu, mem := output.Field(r, "cpuCores"), output.Field(r, "memoryGib")
	if cpu == "" || mem == "" {
		return ""
	}
	return cpu + " vCPU / " + mem + " GiB"
}

func newContainerCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "container",
		Aliases: []string{"containers"},
		Short:   "Manage Xcloud containers",
		Long: "Containers are Linux workloads that each run in their own micro-VM, on the\n" +
			"same networks and security groups as your instances.\n\n" +
			"The image and the size of a container are fixed once it is created; to change\n" +
			"either, create a new container. Containers are offered per region — see\n" +
			"`cloudconsole container catalog`.",
	}
	c.AddCommand(
		&cobra.Command{
			Use: "list", Aliases: []string{"ls"},
			Short: "List containers", Args: cobra.NoArgs,
			RunE: simpleGet(s, containersPath, nil, containerColumns),
		},
		&cobra.Command{
			Use: "get <id>", Short: "Show one container", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return renderGet(cmd, s, containersPath+"/"+args[0], containerColumns)
			},
		},
		&cobra.Command{
			Use:   "catalog",
			Short: "Show where containers are offered, with sizes and prices",
			Args:  cobra.NoArgs,
			RunE:  simpleGet(s, containersPath+"/catalog", nil, containerCatalogColumns),
		},
		newContainerCreateCommand(s),
		newContainerDeleteCommand(s),
		newContainerLogsCommand(s),
		newContainerFileCommand(s),
	)
	c.AddCommand(newContainerLifecycleCommands(s)...)
	return c
}

func containerCatalogColumns(time.Time) []output.Column {
	return []output.Column{
		{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
		{Header: "offered", Value: func(r map[string]any) string { return output.Field(r, "enabled") }},
		{Header: "flavors", Value: func(r map[string]any) string {
			flavors, _ := r["flavors"].([]any)
			names := make([]string, 0, len(flavors))
			for _, f := range flavors {
				if m, ok := f.(map[string]any); ok {
					names = append(names, output.Field(m, "slug"))
				}
			}
			return strings.Join(names, ", ")
		}},
		{Header: "registry", Value: func(r map[string]any) string { return output.Field(r, "managedRegistryHost") }},
	}
}

// fetchContainerState is fetchInstanceState for a container: a 404 means the
// delete has completed.
func fetchContainerState(ctx context.Context, c *api.Client, id string) (wait.State, error) {
	resp, err := c.Do(ctx, api.RequestOptions{Method: http.MethodGet, Path: containersPath + "/" + id})
	if err != nil {
		var p *api.Problem
		if errors.As(err, &p) && (p.Status == http.StatusNotFound || p.Status == http.StatusGone) {
			return wait.State{Exists: false}, nil
		}
		return wait.State{}, err
	}
	var st instanceState
	if err := json.Unmarshal(resp.Body, &st); err != nil {
		return wait.State{}, fmt.Errorf("could not read the container state: %w", err)
	}
	return wait.State{
		Exists:        true,
		Status:        st.Status,
		PendingAction: deref(st.PendingAction),
		LastError:     deref(st.LastError),
	}, nil
}

func awaitContainer(cmd *cobra.Command, s *State, id string, predicate wait.Predicate, flags waitFlags, verb string) error {
	client, err := s.Client()
	if err != nil {
		return err
	}
	return runWait(cmd, s, flags, verb,
		waitTimeoutError{Resource: "container " + id, Check: "cloudconsole container get " + id},
		func(ctx context.Context) (wait.State, error) { return fetchContainerState(ctx, client, id) },
		predicate)
}

func newContainerCreateCommand(s *State) *cobra.Command {
	var (
		name, region, image, flavor, network, registry, restart, eip string
		cpu, memory, storage                                         int
		securityGroups, env, volumes, command                        []string
		flags                                                        waitFlags
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Create and start a container",
		Long: "Create a container from an image and start it.\n\n" +
			"Pick a size with --flavor, or set --cpu, --memory and --storage yourself.\n" +
			"Everything after `--` replaces the image's command.",
		Example: "  cloudconsole container create --name web --region BIT1 --image docker.io/library/nginx:1.27 \\\n" +
			"      --flavor ctr.small --network app --wait\n" +
			"  cloudconsole container create --name job --region BIT1 --image docker.io/library/alpine:3.21 \\\n" +
			"      --cpu 1 --memory 1 --storage 5 --network app --restart never \\\n" +
			"      --env LEVEL=debug --volume <volume-id>:/data -- sh -c 'echo hello > /data/out'",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			custom := cpu > 0 || memory > 0 || storage > 0
			if flavor != "" && custom {
				return &usageError{errors.New("pass --flavor, or --cpu, --memory and --storage, not both")}
			}
			if flavor == "" && (cpu <= 0 || memory <= 0 || storage <= 0) {
				return &usageError{errors.New("pass --flavor, or all of --cpu, --memory and --storage")}
			}
			regionID, err := resolveRegion(cmd.Context(), client, region)
			if err != nil {
				return err
			}
			body := map[string]any{"regionId": regionID, "name": name, "image": image, "network": network}
			if flavor != "" {
				body["flavor"] = flavor
			} else {
				body["cpuCores"], body["memoryGib"], body["writableGib"] = cpu, memory, storage
			}
			if registry != "" {
				body["registryId"] = registry
			}
			if len(securityGroups) > 0 {
				body["securityGroups"] = securityGroups
			}
			if restart != "" {
				body["restartPolicy"] = strings.ReplaceAll(restart, "-", "_")
			}
			if len(command) > 0 {
				body["command"] = command
			} else if len(args) > 0 {
				body["command"] = args
			}
			if len(env) > 0 {
				vars := map[string]string{}
				for _, kv := range env {
					key, value, ok := strings.Cut(kv, "=")
					if !ok || key == "" {
						return &usageError{fmt.Errorf("--env %q is not KEY=value", kv)}
					}
					vars[key] = value
				}
				body["env"] = vars
			}
			if len(volumes) > 0 {
				mounts := make([]map[string]any, 0, len(volumes))
				for _, v := range volumes {
					parts := strings.Split(v, ":")
					if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || !strings.HasPrefix(parts[1], "/") {
						return &usageError{fmt.Errorf("--volume %q is not <volume-id>:<absolute path>[:ro]", v)}
					}
					mount := map[string]any{"volumeId": parts[0], "path": parts[1]}
					if len(parts) == 3 {
						if parts[2] != "ro" {
							return &usageError{fmt.Errorf("--volume %q: the only option is ro", v)}
						}
						mount["readOnly"] = true
					}
					mounts = append(mounts, mount)
				}
				body["volumes"] = mounts
			}
			switch {
			case eip == "":
			case eip == "new":
				body["elasticIp"] = map[string]any{"mode": "allocate"}
			case looksLikeUUID(eip):
				body["elasticIp"] = map[string]any{"mode": "existing", "existingId": eip}
			default:
				return &usageError{errors.New("--elastic-ip takes `new` or the id of an unattached elastic IP")}
			}
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost, Path: containersPath, Body: payload,
				IdempotencyKey: api.NewIdempotencyKey(),
			})
			if err != nil {
				return err
			}
			if flags.enabled {
				var created struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(resp.Body, &created); err != nil || created.ID == "" {
					return fmt.Errorf("could not read the new container's id: %w", err)
				}
				if err := awaitContainer(cmd, s, created.ID, wait.ContainerStarted(), flags, "starting"); err != nil {
					return err
				}
				return renderGet(cmd, s, containersPath+"/"+created.ID, containerColumns)
			}
			return w.Render(resp.Body, containerColumns(time.Now()))
		},
	}
	f := c.Flags()
	f.StringVar(&name, "name", "", "container name, also its hostname (required)")
	f.StringVar(&region, "region", "", "region slug or id (required)")
	f.StringVar(&image, "image", "", "image reference, e.g. docker.io/library/nginx:1.27 (required)")
	f.StringVar(&network, "network", "", "Xcloud network to attach to (required)")
	f.StringVar(&flavor, "flavor", "", "preset size, e.g. ctr.small")
	f.IntVar(&cpu, "cpu", 0, "vCPUs, for a custom size")
	f.IntVar(&memory, "memory", 0, "memory in GiB, for a custom size")
	f.IntVar(&storage, "storage", 0, "writable storage in GiB, for a custom size")
	f.StringVar(&registry, "registry", "", "id of saved credentials for a private registry")
	f.StringSliceVar(&securityGroups, "security-group", nil, "security group name (repeatable)")
	f.StringArrayVar(&env, "env", nil, "environment variable KEY=value (repeatable)")
	f.StringArrayVar(&volumes, "volume", nil, "mount a volume: <volume-id>:<path>[:ro] (repeatable)")
	f.StringArrayVar(&command, "command", nil, "one argument of the command (repeatable); or pass it after --")
	f.StringVar(&restart, "restart", "", "if the process exits: always, on-failure or never")
	f.StringVar(&eip, "elastic-ip", "", "`new`, or the id of an unattached elastic IP")
	for _, r := range []string{"name", "region", "image", "network"} {
		_ = c.MarkFlagRequired(r)
	}
	flags.register(c)
	return c
}

func newContainerLifecycleCommands(s *State) []*cobra.Command {
	actions := []struct {
		action, short, verb string
		predicate           func() wait.Predicate
	}{
		{"start", "Start a stopped container", "starting", wait.ContainerStarted},
		{"stop", "Stop a running container", "stopping", wait.ContainerStopped},
		{"restart", "Stop and start a running container", "restarting", wait.ContainerStarted},
	}
	out := make([]*cobra.Command, 0, len(actions))
	for _, a := range actions {
		a := a
		var flags waitFlags
		c := &cobra.Command{
			Use: a.action + " <id>", Short: a.short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				client, err := s.Client()
				if err != nil {
					return err
				}
				if _, err := client.Do(cmd.Context(), api.RequestOptions{
					Method: http.MethodPost, Path: containersPath + "/" + args[0] + "/" + a.action,
				}); err != nil {
					return err
				}
				if !flags.enabled {
					if w, _ := s.Writer(); w != nil && !w.Quiet {
						fmt.Fprintf(w.Err, "%s queued for container %s. Add --wait to block until it finishes.\n", a.action, args[0])
					}
					return nil
				}
				return awaitContainer(cmd, s, args[0], a.predicate(), flags, a.verb)
			},
		}
		flags.register(c)
		out = append(out, c)
	}
	return out
}

func newContainerDeleteCommand(s *State) *cobra.Command {
	var (
		yes   bool
		flags waitFlags
	)
	c := &cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm"},
		Short:   "Delete a container",
		Long: "Delete a container and its writable storage. Mounted volumes and elastic IPs\n" +
			"are kept and become free again. Billing stops with the request.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				ok, err := s.Confirm(fmt.Sprintf(
					"Delete container %s and its writable storage? This cannot be undone. [y/N]: ", args[0]))
				if err != nil {
					return err
				}
				if !ok {
					if w, _ := s.Writer(); w != nil {
						fmt.Fprintln(w.Err, "Aborted.")
					}
					return nil
				}
			}
			client, err := s.Client()
			if err != nil {
				return err
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodDelete, Path: containersPath + "/" + args[0],
			}); err != nil {
				return err
			}
			if flags.enabled {
				return awaitContainer(cmd, s, args[0], wait.InstanceGone(), flags, "deleting")
			}
			if w, _ := s.Writer(); w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Delete queued for container %s.\n", args[0])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	flags.register(c)
	return c
}

func newContainerLogsCommand(s *State) *cobra.Command {
	var follow bool
	c := &cobra.Command{
		Use:   "logs <id>",
		Short: "Print the container's output",
		Long: "Print what the container has written since its last start, at most the last\n" +
			"256 KiB. With --follow, keep printing new output until interrupted.",
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
			read := func() (string, error) {
				resp, err := client.Do(cmd.Context(), api.RequestOptions{
					Method: http.MethodGet, Path: containersPath + "/" + args[0] + "/logs",
				})
				if err != nil {
					return "", err
				}
				var body struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(resp.Body, &body); err != nil {
					return "", fmt.Errorf("could not read the log: %w", err)
				}
				return body.Text, nil
			}
			printed, err := read()
			if err != nil {
				return err
			}
			fmt.Fprint(w.Out, printed)
			for follow {
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(2 * time.Second):
				}
				text, err := read()
				if err != nil {
					return err
				}
				// The API returns the output so far each time. Print what was
				// added; after a restart the output starts over, so print all.
				if strings.HasPrefix(text, printed) {
					fmt.Fprint(w.Out, text[len(printed):])
				} else {
					fmt.Fprint(w.Out, text)
				}
				printed = text
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new output")
	return c
}

func newContainerFileCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:   "file",
		Short: "Copy a file out of or into a running container",
		Long: "Files up to 8 MiB, from and to the container's own filesystem. Files on\n" +
			"mounted volumes are not reachable this way.",
	}
	var out string
	get := &cobra.Command{
		Use:     "get <id> <path>",
		Short:   "Download a file from the container",
		Example: "  cloudconsole container file get <id> /etc/nginx/nginx.conf --output nginx.conf",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodGet, Path: containersPath + "/" + args[0] + "/files",
				Query: url.Values{"path": {args[1]}},
			})
			if err != nil {
				return err
			}
			if out == "" || out == "-" {
				_, err = w.Out.Write(resp.Body)
				return err
			}
			if err := os.WriteFile(out, resp.Body, 0o600); err != nil {
				return err
			}
			if !w.Quiet {
				fmt.Fprintf(w.Err, "Wrote %d bytes to %s.\n", len(resp.Body), out)
			}
			return nil
		},
	}
	get.Flags().StringVarP(&out, "output", "O", "", "write to this file instead of stdout")

	var mode string
	put := &cobra.Command{
		Use:     "put <id> <local file> <path>",
		Short:   "Upload a file into the container",
		Example: "  cloudconsole container file put <id> ./config.json /etc/app/config.json --mode 640",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(args[1])
			if err != nil {
				return err
			}
			query := url.Values{"path": {args[2]}}
			if mode != "" {
				query.Set("mode", mode)
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPut, Path: containersPath + "/" + args[0] + "/files",
				Query: query, Body: data, ContentType: "application/octet-stream",
			}); err != nil {
				return err
			}
			if w, _ := s.Writer(); w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Wrote %d bytes to %s.\n", len(data), args[2])
			}
			return nil
		},
	}
	put.Flags().StringVar(&mode, "mode", "", "octal file mode, e.g. 644")
	c.AddCommand(get, put)
	return c
}
