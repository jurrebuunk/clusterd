package labctl

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jurrebuunk/clusterd/internal/api"
)

// S3 users and bucket changes. A new key is only ever written to a file
// (mode 600, never overwritten), never to the terminal.

func cmdS3User(e *env) *cobra.Command {
	c := group("user", "Object store users")
	names := e.complete("/v1/s3/users")
	c.AddCommand(listCmd(e, "Users with their buckets and usage", "/v1/s3/users",
		func(u api.S3User) string { return u.Name },
		func(l api.List[api.S3User]) error {
			warnAll(e, "s3 users", l.Errors)
			tw := newTable(e.stdout)
			fmt.Fprintln(tw, "USER\tNAME\tKEYS\tBUCKETS\tOBJECTS\tSIZE\tSTATUS")
			for _, u := range l.Items {
				st := map[bool]string{true: "Suspended", false: "Active"}[u.Suspended]
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\t%s\n", u.Name, u.DisplayName, u.Keys, u.Buckets, u.Objects, bytesHuman(u.SizeBytes), st)
			}
			return tw.Flush()
		}))
	c.AddCommand(&cobra.Command{
		Use: "show USER", Short: "One user", Args: args(1, "USER"), ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			var u api.S3User
			if err := e.get("/v1/s3/users/"+url.PathEscape(a[0]), &u); err != nil {
				return err
			}
			return e.render(u, []string{u.Name}, func() error {
				return e.fields([][2]string{{"User", u.Name}, {"Name", u.DisplayName}, {"Keys", strconv.Itoa(u.Keys)},
					{"Buckets", strconv.Itoa(u.Buckets)}, {"Objects", strconv.FormatInt(u.Objects, 10)}, {"Size", bytesHuman(u.SizeBytes)},
					{"Status", map[bool]string{true: "Suspended", false: "Active"}[u.Suspended]}})
			})
		},
	})

	var display, out string
	create := &cobra.Command{
		Use: "create USER --out FILE", Short: "Create a user; its key goes to FILE", Args: args(1, "USER --out FILE"),
		RunE: func(_ *cobra.Command, a []string) error {
			f, err := keyFile(out)
			if err != nil {
				return err
			}
			cl, err := e.client()
			if err != nil {
				f.discard()
				return err
			}
			var cred api.S3Credentials
			if err := cl.Call(e.ctx, http.MethodPost, "/v1/s3/users", nil, api.S3UserCreate{Name: a[0], DisplayName: display}, &cred); err != nil {
				f.discard()
				return err
			}
			if err := f.write(cred); err != nil {
				return err
			}
			fmt.Fprintf(e.stdout, "user/%s created; its key is in %s\n", cred.User, out)
			return nil
		},
	}
	create.Flags().StringVar(&display, "display-name", "", "a readable name (default: USER)")
	create.Flags().StringVar(&out, "out", "", "write the key to this new file (mode 600)")

	var yes bool
	del := &cobra.Command{
		Use: "delete USER", Short: "Delete a user (refused while it owns buckets)", Args: args(1, "USER"), ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			if !yes {
				if err := e.confirm("Delete S3 user "+a[0], "delete user "+a[0], nil); err != nil {
					return err
				}
			}
			cl, err := e.client()
			if err != nil {
				return err
			}
			if err := cl.Call(e.ctx, http.MethodDelete, "/v1/s3/users/"+url.PathEscape(a[0]), nil, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(e.stdout, "user/%s deleted\n", a[0])
			return nil
		},
	}
	del.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")

	var rotOut string
	var rotYes bool
	rotate := &cobra.Command{
		Use: "rotate-keys USER --out FILE", Short: "Give a user a new key and remove its old ones", Args: args(1, "USER --out FILE"),
		ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			f, err := keyFile(rotOut)
			if err != nil {
				return err
			}
			if !rotYes {
				if err := e.confirm("Replace the keys of "+a[0]+"; whatever uses the old ones stops working", "rotate the keys of "+a[0], nil); err != nil {
					f.discard()
					return err
				}
			}
			cl, err := e.client()
			if err != nil {
				f.discard()
				return err
			}
			var cred api.S3Credentials
			if err := cl.Call(e.ctx, http.MethodPost, "/v1/s3/users/"+url.PathEscape(a[0])+"/keys", nil, nil, &cred); err != nil {
				f.discard()
				return err
			}
			if err := f.write(cred); err != nil {
				return err
			}
			fmt.Fprintf(e.stdout, "user/%s has a new key in %s; the old ones are gone\n", cred.User, rotOut)
			return nil
		},
	}
	rotate.Flags().StringVar(&rotOut, "out", "", "write the new key to this new file (mode 600)")
	rotate.Flags().BoolVarP(&rotYes, "yes", "y", false, "don't ask for confirmation")
	c.AddCommand(create, del, rotate)
	return c
}

// bucketWrites adds create, delete and quota to the bucket group.
func bucketWrites(e *env, b *cobra.Command) {
	names := e.complete("/v1/s3/buckets")
	var owner, quota string
	create := &cobra.Command{
		Use: "create BUCKET --owner USER", Short: "Create a bucket for a user, optionally with --quota", Args: args(1, "BUCKET --owner USER"),
		RunE: func(_ *cobra.Command, a []string) error {
			if owner == "" {
				return usageErr("--owner is required")
			}
			in := api.S3BucketCreate{Name: a[0], Owner: owner}
			if quota != "" {
				n, err := parseSize(quota)
				if err != nil {
					return usageErr("--quota: %v", err)
				}
				in.QuotaBytes = n
			}
			return e.bucketCall(http.MethodPost, "/v1/s3/buckets", in, "created")
		},
	}
	create.Flags().StringVar(&owner, "owner", "", "the user that owns it")
	create.Flags().StringVar(&quota, "quota", "", "size limit, e.g. 10G or 500Mi (default: none)")
	create.RegisterFlagCompletionFunc("owner", e.complete("/v1/s3/users"))

	setQuota := &cobra.Command{
		Use: "quota BUCKET SIZE|none", Short: "Set or remove a bucket's size limit", Args: args(2, "BUCKET SIZE|none"),
		ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			var n int64
			if a[1] != "none" {
				var err error
				if n, err = parseSize(a[1]); err != nil || n == 0 {
					return usageErr("SIZE: like 10G or 500Mi, or none")
				}
			}
			return e.bucketCall(http.MethodPatch, "/v1/s3/buckets/"+url.PathEscape(a[0]), api.S3BucketUpdate{QuotaBytes: &n}, "updated")
		},
	}

	var force, yes bool
	del := &cobra.Command{
		Use: "delete BUCKET", Short: "Delete a bucket (refused while it holds objects, unless --force)", Args: args(1, "BUCKET"),
		ValidArgsFunction: names,
		RunE: func(_ *cobra.Command, a []string) error {
			if !yes {
				if !isTerminal(e.stdin) {
					return usageErr("not a terminal: pass --yes to delete %s", a[0])
				}
				q := "Delete bucket " + a[0]
				if force {
					q += " and everything in it; type its name to confirm"
				}
				fmt.Fprintf(e.stderr, "%s: ", q)
				answer, _ := readLine(e.stdin)
				if (force && answer != a[0]) || (!force && answer != "y" && answer != "yes") {
					return &exitError{ExitAPI, fmt.Errorf("aborted")}
				}
			}
			q := url.Values{}
			if force {
				q.Set("force", "true")
			}
			cl, err := e.client()
			if err != nil {
				return err
			}
			if err := cl.Call(e.ctx, http.MethodDelete, "/v1/s3/buckets/"+url.PathEscape(a[0]), q, nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(e.stdout, "bucket/%s deleted\n", a[0])
			return nil
		},
	}
	del.Flags().BoolVar(&force, "force", false, "also delete the objects in it")
	del.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	b.AddCommand(create, setQuota, del)
}

func (e *env) bucketCall(method, path string, in any, done string) error {
	cl, err := e.client()
	if err != nil {
		return err
	}
	var b api.S3Bucket
	if err := cl.Call(e.ctx, method, path, nil, in, &b); err != nil {
		return err
	}
	if e.json() {
		return e.printJSON(b)
	}
	quota := "no quota"
	if b.QuotaBytes > 0 {
		quota = "quota " + bytesHuman(b.QuotaBytes)
	}
	fmt.Fprintf(e.stdout, "bucket/%s %s (owner %s, %s)\n", b.Name, done, b.Owner, quota)
	return nil
}

// keyOut is a new, empty mode-600 file for a key, made before asking the
// API so a key is never created without a place to put it.
type keyOut struct {
	f *os.File
}

func keyFile(path string) (*keyOut, error) {
	if path == "" {
		return nil, usageErr("--out FILE is required: keys go to a file, never to the terminal")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, usageErr("--out: %v", err)
	}
	return &keyOut{f}, nil
}

// write stores the key as an env file the aws CLI and SDKs understand.
func (k *keyOut) write(c api.S3Credentials) error {
	_, err := fmt.Fprintf(k.f, "AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\nAWS_ENDPOINT_URL=%s\n", c.AccessKey, c.SecretKey, c.Endpoint)
	if cerr := k.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (k *keyOut) discard() {
	k.f.Close()
	os.Remove(k.f.Name())
}

// parseSize reads 10G, 1.5T, 500M (powers of 1000) or 10Gi, 500Mi (powers
// of 1024); a trailing B is allowed.
func parseSize(s string) (int64, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "B")
	mult := 1.0
	for _, u := range []struct {
		suffix string
		m      float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}} {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = n, u.m
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("not a size: %q", s)
	}
	return int64(v * mult), nil
}
