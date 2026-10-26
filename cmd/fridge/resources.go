package main

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---- database credentials chosen on the command line ----

const (
	lowerDigits = "abcdefghijklmnopqrstuvwxyz0123456789"
	passwordSet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
)

// randomString draws n characters of alphabet with crypto/rand, without modulo bias.
func randomString(alphabet string, n int) string {
	out := make([]byte, 0, n)
	max := byte(256 - 256%len(alphabet))
	buf := make([]byte, 1)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		if buf[0] < max {
			out = append(out, alphabet[int(buf[0])%len(alphabet)])
		}
	}
	return string(out)
}

type credFlags struct {
	user      string
	pwStdin   bool
	random    bool
	generated bool // user and password were made here and must be shown once
}

func (o *credFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.user, "db-user", "", "database user suffix (the account name is prepended)")
	fs.BoolVar(&o.pwStdin, "db-password-stdin", false, "read the database password from stdin")
	fs.BoolVar(&o.random, "random-credentials", false, "generate the database user and password (shown once)")
}

// resolve returns the user suffix and password to send (both may be empty: the server picks).
func (o *credFlags) resolve(c *cli, cmd string) (user, password string, err error) {
	if o.random && (o.user != "" || o.pwStdin) {
		return "", "", usageErr(cmd + ": --random-credentials excludes --db-user and --db-password-stdin")
	}
	if o.random {
		o.generated = true
		return randomString(lowerDigits, 8), randomString(passwordSet, 24), nil
	}
	if o.pwStdin {
		if password, err = c.readLine(); err != nil {
			return "", "", err
		}
	}
	return o.user, password, nil
}

// announce shows generated credentials; they exist nowhere else.
func (o *credFlags) announce(c *cli, user, password string) {
	if o.generated {
		fmt.Fprintf(c.out, "db user suffix: %s\ndb password: %s\n(shown once)\n", user, password)
	}
}

// ---- create / show ----

func (c *cli) create(args []string) error {
	var domain, version string
	var reveal bool
	var cf credFlags
	pos, err := c.need("create", args, 2, func(fs *flag.FlagSet) {
		fs.StringVar(&domain, "domain", "", "a domain you registered (required)")
		fs.StringVar(&version, "version", "", "engine version")
		fs.BoolVar(&reveal, "reveal", false, "print the connection string with the password")
		cf.register(fs)
	})
	if err != nil {
		return err
	}
	if domain == "" {
		return usageErr("create: --domain is required")
	}
	dbUser, dbPass, err := cf.resolve(c, "create")
	if err != nil {
		return err
	}
	body := map[string]string{"engine": pos[0], "name": pos[1], "domain": domain, "version": version}
	if dbUser != "" {
		body["db_user"] = dbUser
	}
	if dbPass != "" {
		body["db_password"] = dbPass
	}
	b, err := call("POST", "/api/databases", body)
	if err != nil {
		return err
	}
	var v struct {
		Name             string
		ConnectionString string `json:"connection_string"`
	}
	return c.show(b, &v, func() {
		cs := v.ConnectionString
		if reveal {
			if cs, err = c.revealConn(v.Name); err != nil {
				fmt.Fprintln(c.errw, "warning: could not reveal the connection string:", err)
				cs = v.ConnectionString
			}
		}
		fmt.Fprintf(c.out, "created %s\n%s\n", v.Name, cs)
		cf.announce(c, dbUser, dbPass)
	})
}

// revealConn fetches the application connection string with its password (recorded in the audit log).
func (c *cli) revealConn(db string) (string, error) {
	b, err := call("GET", "/api/databases/"+url.PathEscape(db)+"/connection", nil)
	if err != nil {
		return "", err
	}
	var v struct {
		ConnectionString string `json:"connection_string"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	return v.ConnectionString, nil
}

func (c *cli) showDB(args []string) error {
	var reveal bool
	pos, err := c.need("show", args, 1, func(fs *flag.FlagSet) {
		fs.BoolVar(&reveal, "reveal", false, "print the connection string with the password (recorded in the audit log)")
	})
	if err != nil {
		return err
	}
	b, err := call("GET", "/api/databases/"+url.PathEscape(pos[0]), nil)
	if err != nil {
		return err
	}
	var d dbT
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	if reveal {
		if d.ConnectionString, err = c.revealConn(pos[0]); err != nil {
			return err
		}
		if c.json {
			return json.NewEncoder(c.out).Encode(map[string]any{"id": d.ID, "name": d.Name, "connection_string": d.ConnectionString})
		}
	} else if c.json {
		fmt.Fprintln(c.out, strings.TrimSpace(string(b)))
		return nil
	}
	rows := [][]string{{"ID", itoa(d.ID)}, {"NAME", d.Name}, {"ENGINE", d.Engine + " " + d.Version},
		{"DOMAIN", d.Domain}, {"STATUS", d.Status}, {"CONNECTION", d.ConnectionString}}
	c.table(rows)
	return nil
}

// ---- superuser ----

func (c *cli) superuser(args []string) error {
	if len(args) == 0 {
		return usageErr("superuser: missing subcommand (show | rotate)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "show":
		var reveal bool
		pos, err := c.need("superuser show", rest, 1, func(fs *flag.FlagSet) {
			fs.BoolVar(&reveal, "reveal", false, "print the superuser's password too (recorded in the audit log)")
		})
		if err != nil {
			return err
		}
		db := url.PathEscape(pos[0])
		if !reveal { // the name is on the database itself; nothing secret is fetched
			b, err := call("GET", "/api/databases/"+db, nil)
			if err != nil {
				return err
			}
			var v struct {
				AdminUser string `json:"admin_user"`
			}
			return c.show(b, &v, func() { fmt.Fprintln(c.out, v.AdminUser) })
		}
		b, err := call("POST", "/api/databases/"+db+"/superuser/reveal", nil)
		if err != nil {
			return err
		}
		var v struct{ User, Password string }
		return c.show(b, &v, func() { c.table([][]string{{"USER", v.User}, {"PASSWORD", v.Password}}) })
	case "rotate":
		pos, err := c.need("superuser rotate", rest, 1, nil)
		if err != nil {
			return err
		}
		if _, err := call("POST", "/api/databases/"+url.PathEscape(pos[0])+"/superuser/rotate", nil); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "superuser password rotated")
		return nil
	}
	return usageErr("superuser: unknown subcommand " + strconv.Quote(sub))
}

// ---- members ----

func (c *cli) members(args []string) error {
	if len(args) == 0 {
		return usageErr("members: missing subcommand (ls | add | rm)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls":
		pos, err := c.need("members ls", rest, 1, nil)
		if err != nil {
			return err
		}
		b, err := call("GET", "/api/databases/"+url.PathEscape(pos[0])+"/members", nil)
		if err != nil {
			return err
		}
		var v []struct {
			UserID   int64     `json:"user_id"`
			Username string    `json:"username"`
			AddedAt  time.Time `json:"added_at"`
		}
		return c.show(b, &v, func() {
			rows := [][]string{{"ID", "USER", "ADDED"}}
			for _, m := range v {
				rows = append(rows, []string{itoa(m.UserID), m.Username, m.AddedAt.Format(time.RFC3339)})
			}
			c.table(rows)
		})
	case "add":
		pos, err := c.need("members add", rest, 2, nil)
		if err != nil {
			return err
		}
		if _, err := call("POST", "/api/databases/"+url.PathEscape(pos[0])+"/members", map[string]string{"username": pos[1]}); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "%s is now a member of %s\n", pos[1], pos[0])
		return nil
	case "rm":
		pos, err := c.need("members rm", rest, 2, nil)
		if err != nil {
			return err
		}
		db := url.PathEscape(pos[0])
		b, err := call("GET", "/api/databases/"+db+"/members", nil)
		if err != nil {
			return err
		}
		var ms []struct {
			UserID   int64  `json:"user_id"`
			Username string `json:"username"`
		}
		if err := json.Unmarshal(b, &ms); err != nil {
			return err
		}
		for _, m := range ms {
			if m.Username == pos[1] {
				if _, err := call("DELETE", "/api/databases/"+db+"/members/"+itoa(m.UserID), nil); err != nil {
					return err
				}
				fmt.Fprintf(c.out, "%s is no longer a member of %s\n", pos[1], pos[0])
				return nil
			}
		}
		return fmt.Errorf("%s is not a member of %s", pos[1], pos[0])
	}
	return usageErr("members: unknown subcommand " + strconv.Quote(sub))
}

// ---- domains ----

type domainT struct {
	ID    int64
	Name  string
	Owner string
	InUse int `json:"in_use"`
}

func (c *cli) domains(args []string) error {
	if len(args) == 0 {
		return usageErr("domains: missing subcommand (ls | add | rm)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls":
		if _, err := c.need("domains ls", rest, 0, nil); err != nil {
			return err
		}
		b, err := call("GET", "/api/domains", nil)
		if err != nil {
			return err
		}
		var v []domainT
		return c.show(b, &v, func() {
			rows := [][]string{{"ID", "DOMAIN", "OWNER", "DATABASES"}}
			for _, d := range v {
				rows = append(rows, []string{itoa(d.ID), d.Name, d.Owner, strconv.Itoa(d.InUse)})
			}
			c.table(rows)
		})
	case "add":
		pos, err := c.need("domains add", rest, 1, nil)
		if err != nil {
			return err
		}
		b, err := call("POST", "/api/domains", map[string]string{"name": pos[0]})
		if err != nil {
			return err
		}
		var v domainT
		return c.show(b, &v, func() { fmt.Fprintf(c.out, "domain %s registered (id=%d)\n", v.Name, v.ID) })
	case "rm":
		pos, err := c.need("domains rm", rest, 1, nil)
		if err != nil {
			return err
		}
		b, err := call("GET", "/api/domains", nil)
		if err != nil {
			return err
		}
		var ds []domainT
		if err := json.Unmarshal(b, &ds); err != nil {
			return err
		}
		for _, d := range ds {
			if strings.EqualFold(d.Name, pos[0]) {
				if _, err := call("DELETE", "/api/domains/"+itoa(d.ID), nil); err != nil {
					return err
				}
				fmt.Fprintf(c.out, "domain %s removed\n", d.Name)
				return nil
			}
		}
		return fmt.Errorf("domain %q not found", pos[0])
	}
	return usageErr("domains: unknown subcommand " + strconv.Quote(sub))
}

// ---- dest share, check ----

func (c *cli) destShare(args []string) error {
	pos, err := c.need("dest share", args, 2, nil)
	if err != nil {
		return err
	}
	var on bool
	switch pos[1] {
	case "on":
		on = true
	case "off":
	default:
		return usageErr("dest share: expected on or off")
	}
	id, err := destID(pos[0])
	if err != nil {
		return err
	}
	if _, err := call("PATCH", "/api/destinations/"+itoa(id), map[string]bool{"shared": on}); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "destination %s shared: %s\n", pos[0], pos[1])
	return nil
}

func (c *cli) check(args []string) error {
	var from string
	var wait bool
	timeout := 6 * time.Hour
	if _, err := c.need("check", args, 0, func(fs *flag.FlagSet) {
		fs.StringVar(&from, "from", "", "<dest>:<key> (required)")
		fs.BoolVar(&wait, "wait", false, "wait until finished")
		fs.DurationVar(&timeout, "timeout", timeout, "max time to wait with --wait")
	}); err != nil {
		return err
	}
	d, key, err := parseFrom(from)
	if err != nil {
		return usageErr(err.Error())
	}
	id, err := destID(d)
	if err != nil {
		return err
	}
	b, err := call("POST", "/api/dump-checks", map[string]any{"destination_id": id, "key": key})
	if err != nil {
		return err
	}
	return c.job(b, "/api/dump-checks/", "GET /api/dump-checks/<id>", wait, timeout)
}

// ---- audit ----

// sinceDuration is time.ParseDuration plus a "d" (days) suffix.
func sinceDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return d, nil
}

func (c *cli) audit(args []string) error {
	var user, action, db, since string
	var csv bool
	if _, err := c.need("audit", args, 0, func(fs *flag.FlagSet) {
		fs.StringVar(&user, "user", "", "only this account's actions")
		fs.StringVar(&action, "action", "", "action prefix, e.g. db.")
		fs.StringVar(&db, "db", "", "only this database (name or id)")
		fs.StringVar(&since, "since", "", "only the last period, e.g. 24h or 7d")
		fs.BoolVar(&csv, "csv", false, "print CSV")
	}); err != nil {
		return err
	}
	q := url.Values{}
	for k, v := range map[string]string{"actor": user, "action": action, "db": db} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if since != "" {
		d, err := sinceDuration(since)
		if err != nil {
			return usageErr("audit: " + err.Error())
		}
		q.Set("from", time.Now().Add(-d).UTC().Format(time.RFC3339))
	}
	path := "/api/audit"
	if csv {
		path += ".csv"
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	b, err := call("GET", path, nil)
	if err != nil {
		return err
	}
	if csv {
		_, err = c.out.Write(b)
		return err
	}
	var v struct {
		Entries []struct {
			At         time.Time
			Actor      string
			Action     string
			TargetType string `json:"target_type"`
			TargetID   string `json:"target_id"`
			DB         string
			Success    bool
		}
	}
	return c.show(b, &v, func() {
		rows := [][]string{{"WHEN", "WHO", "ACTION", "TARGET", "DATABASE", "OK"}}
		for _, e := range v.Entries {
			target := e.TargetType
			if e.TargetID != "" {
				target += ":" + e.TargetID
			}
			rows = append(rows, []string{e.At.Local().Format("2006-01-02 15:04:05"), e.Actor, e.Action, target, e.DB, strconv.FormatBool(e.Success)})
		}
		c.table(rows)
	})
}
