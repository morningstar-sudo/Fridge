package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `usage: fridge <command> [args]
  login [--user U] [--password-stdin] [--yes] [--ca-file F] | logout | whoami
  setup --setup-token T --user U [--password-stdin]        (first admin, no browser needed)
  engines | ls | backups <db> | ca [--db <db>]       (--db: the CA that vouches for that database; trust it with sslmode=verify-full)
  create <engine> <name> --domain D [--version V] [--reveal] [--db-user U] [--db-password-stdin | --random-credentials]
  show <db> [--reveal]                                   (--reveal prints the password; it is audited)
  superuser show <db> [--reveal] | superuser rotate <db>
  members ls|add|rm <db> [user]
  domains ls | add <name> | rm <name>
  dest add|ls|test|rm|share ...                          (dest share <dest> on|off: admins only)
  objects <dest> [--prefix P] [--q Q]
  schedule add|ls|rm ...
  backup <db> [--dest D] [--wait]
  restore --from <dest>:<key> --name N --domain D [--version V] [--engine E] [--wait] [credential flags as for create]
  check --from <dest>:<key> [--wait]                     (deep check of a backup file with pg_restore)
  audit [--user U] [--action A] [--db D] [--since 24h] [--csv]
  allowlist [set <cidr>...]
  recover allowlist <cidr>... | reset-password <user> | promote <user>   (run inside the server container)
All commands accept --json. Env: FRIDGE_URL (default https://localhost:8443), FRIDGE_SESSION, FRIDGE_CA_FILE.`

type usageErr string

func (e usageErr) Error() string { return string(e) }

var pollEvery = 2 * time.Second

// cliStdin is where prompts and --password-stdin read from; tests replace it.
var cliStdin io.Reader = os.Stdin

type cli struct {
	out, errw io.Writer
	in        io.Reader
	rd        *bufio.Reader
	json      bool
}

// parseFrom splits "<dest>:<key>" at the first colon.
func parseFrom(s string) (dest, key string, err error) {
	dest, key, ok := strings.Cut(s, ":")
	if !ok || dest == "" || key == "" {
		return "", "", fmt.Errorf("--from must be <dest>:<key>, got %q", s)
	}
	return dest, key, nil
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	c := &cli{out: stdout, errw: stderr, in: cliStdin}
	err := c.dispatch(args)
	var ue usageErr
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &ue):
		fmt.Fprintln(stderr, "fridge:", ue)
		fmt.Fprintln(stderr, usage)
		return 2
	}
	fmt.Fprintln(stderr, "fridge:", err)
	return 1
}

// parse runs fs over args allowing flags before and after positionals.
func (c *cli) parse(name string, args []string, setup func(fs *flag.FlagSet)) ([]string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(c.errw)
	fs.BoolVar(&c.json, "json", false, "print raw JSON")
	if setup != nil {
		setup(fs)
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageErr(err.Error())
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// need parses and requires exactly n positionals (n<0: at least -n).
func (c *cli) need(name string, args []string, n int, setup func(fs *flag.FlagSet)) ([]string, error) {
	pos, err := c.parse(name, args, setup)
	if err != nil {
		return nil, err
	}
	if (n >= 0 && len(pos) != n) || (n < 0 && len(pos) < -n) {
		return nil, usageErr(name + ": wrong number of arguments")
	}
	return pos, nil
}

func (c *cli) table(rows [][]string) {
	tw := tabwriter.NewWriter(c.out, 0, 4, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// show prints raw JSON with --json, otherwise calls f with the decoded body.
func (c *cli) show(b []byte, v any, f func()) error {
	if c.json {
		fmt.Fprintln(c.out, strings.TrimSpace(string(b)))
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return err
	}
	f()
	return nil
}

func (c *cli) dispatch(args []string) error {
	if len(args) == 0 {
		return usageErr("missing command")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "engines":
		if _, err := c.need(cmd, rest, 0, nil); err != nil {
			return err
		}
		b, err := call("GET", "/api/engines", nil)
		if err != nil {
			return err
		}
		var v []struct {
			Name     string
			Versions []string
			Default  string
		}
		return c.show(b, &v, func() {
			rows := [][]string{{"ENGINE", "VERSIONS", "DEFAULT"}}
			for _, e := range v {
				rows = append(rows, []string{e.Name, strings.Join(e.Versions, ","), e.Default})
			}
			c.table(rows)
		})
	case "create":
		return c.create(rest)
	case "ls":
		if _, err := c.need(cmd, rest, 0, nil); err != nil {
			return err
		}
		b, err := call("GET", "/api/databases", nil)
		if err != nil {
			return err
		}
		var v []dbT
		return c.show(b, &v, func() {
			rows := [][]string{{"ID", "NAME", "ENGINE", "VERSION", "DOMAIN", "STATUS"}}
			for _, d := range v {
				rows = append(rows, []string{itoa(d.ID), d.Name, d.Engine, d.Version, d.Domain, d.Status})
			}
			c.table(rows)
		})
	case "show":
		return c.showDB(rest)
	case "superuser":
		return c.superuser(rest)
	case "members":
		return c.members(rest)
	case "domains":
		return c.domains(rest)
	case "check":
		return c.check(rest)
	case "audit":
		return c.audit(rest)
	case "dest":
		return c.dest(rest)
	case "objects":
		var prefix, q string
		pos, err := c.need(cmd, rest, 1, func(fs *flag.FlagSet) {
			fs.StringVar(&prefix, "prefix", "", "key prefix")
			fs.StringVar(&q, "q", "", "substring filter")
		})
		if err != nil {
			return err
		}
		id, err := destID(pos[0])
		if err != nil {
			return err
		}
		b, err := call("GET", "/api/destinations/"+itoa(id)+"/objects?"+url.Values{"prefix": {prefix}, "q": {q}}.Encode(), nil)
		if err != nil {
			return err
		}
		var v struct {
			Objects []struct {
				Key          string
				Size         int64
				LastModified time.Time `json:"last_modified"`
			}
			Truncated bool
		}
		return c.show(b, &v, func() {
			rows := [][]string{{"KEY", "SIZE", "MODIFIED"}}
			for _, o := range v.Objects {
				rows = append(rows, []string{o.Key, itoa(o.Size), o.LastModified.Format(time.RFC3339)})
			}
			c.table(rows)
			if v.Truncated {
				fmt.Fprintln(c.errw, "(results truncated; narrow with --prefix/--q)")
			}
		})
	case "schedule":
		return c.schedule(rest)
	case "backup":
		var dest string
		var wait bool
		timeout := 6 * time.Hour
		pos, err := c.need(cmd, rest, 1, func(fs *flag.FlagSet) {
			fs.StringVar(&dest, "dest", "", "destination name or id (default: schedule/first)")
			fs.BoolVar(&wait, "wait", false, "wait until finished")
			fs.DurationVar(&timeout, "timeout", timeout, "max time to wait with --wait")
		})
		if err != nil {
			return err
		}
		body := map[string]int64{}
		if dest != "" {
			if body["destination_id"], err = destID(dest); err != nil {
				return err
			}
		}
		b, err := call("POST", "/api/databases/"+url.PathEscape(pos[0])+"/backups", body)
		if err != nil {
			return err
		}
		return c.job(b, "/api/backups/", "fridge backups "+pos[0], wait, timeout)
	case "backups":
		pos, err := c.need(cmd, rest, 1, nil)
		if err != nil {
			return err
		}
		b, err := call("GET", "/api/databases/"+url.PathEscape(pos[0])+"/backups", nil)
		if err != nil {
			return err
		}
		var v []struct {
			ID        int64
			Status    string
			Trigger   string
			S3Key     string    `json:"s3_key"`
			SizeBytes int64     `json:"size_bytes"`
			StartedAt time.Time `json:"started_at"`
		}
		return c.show(b, &v, func() {
			rows := [][]string{{"ID", "STATUS", "TRIGGER", "SIZE", "STARTED", "KEY"}}
			for _, x := range v {
				rows = append(rows, []string{itoa(x.ID), x.Status, x.Trigger, itoa(x.SizeBytes), x.StartedAt.Format(time.RFC3339), x.S3Key})
			}
			c.table(rows)
		})
	case "restore":
		var from, name, domain, version, eng string
		var wait bool
		var cf credFlags
		timeout := 6 * time.Hour
		_, err := c.need(cmd, rest, 0, func(fs *flag.FlagSet) {
			fs.StringVar(&from, "from", "", "<dest>:<key> (required)")
			fs.StringVar(&name, "name", "", "new database name (required)")
			fs.StringVar(&domain, "domain", "", "new database domain (required)")
			fs.StringVar(&version, "version", "", "engine version")
			fs.StringVar(&eng, "engine", "", "engine")
			fs.BoolVar(&wait, "wait", false, "wait until finished")
			fs.DurationVar(&timeout, "timeout", timeout, "max time to wait with --wait")
			cf.register(fs)
		})
		if err != nil {
			return err
		}
		if name == "" || domain == "" {
			return usageErr("restore: --name and --domain are required")
		}
		d, key, err := parseFrom(from)
		if err != nil {
			return usageErr(err.Error())
		}
		id, err := destID(d)
		if err != nil {
			return err
		}
		dbUser, dbPass, err := cf.resolve(c, "restore")
		if err != nil {
			return err
		}
		nw := map[string]string{"name": name, "domain": domain, "version": version}
		if dbUser != "" {
			nw["db_user"] = dbUser
		}
		if dbPass != "" {
			nw["db_password"] = dbPass
		}
		b, err := call("POST", "/api/restores", map[string]any{"destination_id": id, "key": key, "engine": eng, "new": nw})
		if err != nil {
			return err
		}
		cf.announce(c, dbUser, dbPass)
		return c.job(b, "/api/restores/", "GET /api/restores/<id>", wait, timeout)
	case "allowlist":
		if len(rest) > 0 && rest[0] == "set" {
			pos, err := c.need("allowlist set", rest[1:], -1, nil)
			if err != nil {
				return err
			}
			b, err := call("PUT", "/api/settings/allowlist", map[string][]string{"cidrs": pos})
			if err != nil {
				return err
			}
			return c.cidrs(b)
		}
		if _, err := c.need(cmd, rest, 0, nil); err != nil {
			return err
		}
		b, err := call("GET", "/api/settings/allowlist", nil)
		if err != nil {
			return err
		}
		return c.cidrs(b)
	case "ca":
		var db string
		if _, err := c.need(cmd, rest, 0, func(fs *flag.FlagSet) {
			fs.StringVar(&db, "db", "", "the CA of this database (your own CA, which signed its certificate); without it, the instance CA that signs the web UI")
		}); err != nil {
			return err
		}
		path := "/api/ca"
		if db != "" {
			path = "/api/databases/" + url.PathEscape(db) + "/ca"
		}
		b, err := call("GET", path, nil)
		if err != nil {
			return err
		}
		c.out.Write(b)
		return nil
	case "login":
		return c.login(rest)
	case "logout":
		return c.logout(rest)
	case "whoami":
		return c.whoami(rest)
	case "setup":
		return c.setup(rest)
	case "recover":
		return c.recover(rest)
	case "help", "-h", "--help":
		fmt.Fprintln(c.out, usage)
		return nil
	}
	return usageErr("unknown command " + strconv.Quote(cmd))
}

type dbT struct {
	ID               int64
	Name             string
	Engine           string
	Version          string
	Domain           string
	Status           string
	ConnectionString string `json:"connection_string"`
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (c *cli) cidrs(b []byte) error {
	var v struct{ CIDRs []string }
	return c.show(b, &v, func() {
		for _, x := range v.CIDRs {
			fmt.Fprintln(c.out, x)
		}
	})
}

// destID resolves a destination name or numeric id to an id.
func destID(s string) (int64, error) {
	if id, err := strconv.ParseInt(s, 10, 64); err == nil {
		return id, nil
	}
	b, err := call("GET", "/api/destinations", nil)
	if err != nil {
		return 0, err
	}
	var ds []struct {
		ID   int64
		Name string
	}
	if err := json.Unmarshal(b, &ds); err != nil {
		return 0, err
	}
	for _, d := range ds {
		if d.Name == s {
			return d.ID, nil
		}
	}
	return 0, fmt.Errorf("destination %q not found", s)
}

// job prints a backup/restore start response and, with wait, polls until it leaves "running".
func (c *cli) job(b []byte, getPath, hint string, wait bool, timeout time.Duration) error {
	var v struct {
		ID     int64
		Status string
		Error  string
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	fails := 0
	for wait && v.Status == "running" {
		abort := func(why string) error {
			return fmt.Errorf("%s while waiting for job id=%d; check with `%s`", why, v.ID, hint)
		}
		if time.Now().After(deadline) {
			return abort("timeout")
		}
		time.Sleep(pollEvery)
		nb, err := call("GET", getPath+itoa(v.ID), nil)
		if err != nil {
			if fails++; fails >= 5 {
				return abort(err.Error())
			}
			continue
		}
		fails = 0
		b = nb
		v.Error = ""
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
	}
	if c.json {
		fmt.Fprintln(c.out, strings.TrimSpace(string(b)))
	} else {
		fmt.Fprintf(c.out, "id=%d status=%s\n", v.ID, v.Status)
	}
	if wait && v.Status == "failed" {
		return fmt.Errorf("%s: %s", v.Status, v.Error)
	}
	return nil
}

func (c *cli) dest(args []string) error {
	if len(args) == 0 {
		return usageErr("dest: missing subcommand")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		var endpoint, bucket, ak, sk, region, provider string
		var pathStyle bool
		pos, err := c.need("dest add", rest, 1, func(fs *flag.FlagSet) {
			fs.StringVar(&endpoint, "endpoint", "", "S3 endpoint URL")
			fs.StringVar(&bucket, "bucket", "", "bucket")
			fs.StringVar(&ak, "access-key", "", "access key")
			fs.StringVar(&sk, "secret-key", "", "secret key")
			fs.StringVar(&region, "region", "", "region")
			fs.StringVar(&provider, "provider", "", "provider")
			fs.BoolVar(&pathStyle, "path-style", false, "path-style addressing")
		})
		if err != nil {
			return err
		}
		if endpoint == "" || bucket == "" || ak == "" || sk == "" {
			return usageErr("dest add: --endpoint, --bucket, --access-key, --secret-key are required")
		}
		b, err := call("POST", "/api/destinations", map[string]any{"name": pos[0], "provider": provider, "endpoint": endpoint,
			"region": region, "bucket": bucket, "access_key": ak, "secret_key": sk, "path_style": pathStyle})
		if err != nil {
			return err
		}
		var v struct {
			ID   int64
			Name string
		}
		return c.show(b, &v, func() { fmt.Fprintf(c.out, "destination %s created (id=%d)\n", v.Name, v.ID) })
	case "ls":
		if _, err := c.need("dest ls", rest, 0, nil); err != nil {
			return err
		}
		b, err := call("GET", "/api/destinations", nil)
		if err != nil {
			return err
		}
		var v []struct {
			ID                               int64
			Name, Provider, Endpoint, Bucket string
		}
		return c.show(b, &v, func() {
			rows := [][]string{{"ID", "NAME", "PROVIDER", "ENDPOINT", "BUCKET"}}
			for _, d := range v {
				rows = append(rows, []string{itoa(d.ID), d.Name, d.Provider, d.Endpoint, d.Bucket})
			}
			c.table(rows)
		})
	case "share":
		return c.destShare(rest)
	case "test", "rm":
		pos, err := c.need("dest "+sub, rest, 1, nil)
		if err != nil {
			return err
		}
		id, err := destID(pos[0])
		if err != nil {
			return err
		}
		method, path := "DELETE", "/api/destinations/"+itoa(id)
		if sub == "test" {
			method, path = "POST", path+"/test"
		}
		b, err := call(method, path, nil)
		if err != nil {
			return err
		}
		if c.json && len(b) > 0 {
			fmt.Fprintln(c.out, strings.TrimSpace(string(b)))
		} else {
			fmt.Fprintln(c.out, "ok")
		}
		return nil
	}
	return usageErr("dest: unknown subcommand " + strconv.Quote(sub))
}

func (c *cli) schedule(args []string) error {
	if len(args) == 0 {
		return usageErr("schedule: missing subcommand")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		var dest, cron, prefix string
		var keep int
		pos, err := c.need("schedule add", rest, 1, func(fs *flag.FlagSet) {
			fs.StringVar(&dest, "dest", "", "destination name or id (required)")
			fs.StringVar(&cron, "cron", "0 2 * * *", "cron expression")
			fs.StringVar(&prefix, "prefix", "fridge", "S3 key prefix")
			fs.IntVar(&keep, "keep", 7, "backups to keep")
		})
		if err != nil {
			return err
		}
		if dest == "" {
			return usageErr("schedule add: --dest is required")
		}
		id, err := destID(dest)
		if err != nil {
			return err
		}
		b, err := call("POST", "/api/databases/"+url.PathEscape(pos[0])+"/schedules",
			map[string]any{"destination_id": id, "cron": cron, "prefix": prefix, "keep_latest": keep, "enabled": true})
		if err != nil {
			return err
		}
		var v struct{ ID int64 }
		return c.show(b, &v, func() { fmt.Fprintf(c.out, "schedule created (id=%d)\n", v.ID) })
	case "ls":
		pos, err := c.need("schedule ls", rest, 1, nil)
		if err != nil {
			return err
		}
		b, err := call("GET", "/api/databases/"+url.PathEscape(pos[0])+"/schedules", nil)
		if err != nil {
			return err
		}
		var v []struct {
			ID            int64
			DestinationID int64 `json:"destination_id"`
			Cron, Prefix  string
			KeepLatest    int `json:"keep_latest"`
			Enabled       bool
		}
		return c.show(b, &v, func() {
			rows := [][]string{{"ID", "DEST", "CRON", "PREFIX", "KEEP", "ENABLED"}}
			for _, s := range v {
				rows = append(rows, []string{itoa(s.ID), itoa(s.DestinationID), s.Cron, s.Prefix, strconv.Itoa(s.KeepLatest), strconv.FormatBool(s.Enabled)})
			}
			c.table(rows)
		})
	case "rm":
		pos, err := c.need("schedule rm", rest, 1, nil)
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return usageErr("schedule rm: id must be a number")
		}
		if _, err := call("DELETE", "/api/schedules/"+itoa(id), nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "ok")
		return nil
	}
	return usageErr("schedule: unknown subcommand " + strconv.Quote(sub))
}
