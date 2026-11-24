package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/jurrebuunk/clusterd/internal/api"
	"github.com/jurrebuunk/clusterd/internal/s3admin"
)

// S3 users and buckets. Reads need read, every write manage. The API's own
// user ("clusterd") is left out of lists and can't be changed through it.
// Every write leaves an audit record, so it shows in /v1/operations.

const ownS3User = "clusterd"

var (
	validS3User   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	validS3Bucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

func (s *Server) rgw() (*s3admin.Client, error) {
	if s.cfg.S3.Credentials == "" {
		return nil, errors.New("the S3 admin API is not configured")
	}
	cred, err := s3admin.LoadCredentials(s.cfg.S3.Credentials)
	if err != nil {
		return nil, err
	}
	var gws []string
	for _, h := range s.cfg.LocalFirst() {
		if h.RGW != "" {
			gws = append(gws, h.RGW)
		}
	}
	return &s3admin.Client{Gateways: gws, Cred: cred, HTTP: &http.Client{Timeout: 15 * time.Second}}, nil
}

// audit records a write like the actions do, as one finished operation.
func (s *Server) audit(w http.ResponseWriter, r *http.Request, action, target string, err error) {
	c := callerFrom(r.Context())
	args := []any{"action", action, "target", target, "token", c.token.Name, "scope", c.token.Scope.String(),
		"request_id", w.Header().Get("X-Request-Id"), "event", "end"}
	if err != nil {
		s.log.Error("audit", append(args, "result", "error", "error", err.Error())...)
		return
	}
	s.log.Info("audit", append(args, "result", "ok")...)
}

// rgwError maps an RGW answer to ours.
func rgwError(w http.ResponseWriter, err error) {
	var se *s3admin.StatusError
	switch {
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		writeError(w, http.StatusNotFound, "not found: "+se.Code)
	case errors.As(err, &se) && se.Status == http.StatusConflict:
		writeError(w, http.StatusConflict, "already exists or not empty: "+se.Code)
	case errors.As(err, &se) && se.Status == http.StatusForbidden:
		writeError(w, http.StatusBadGateway, "the gateway refused: "+se.Code+" (does the RGW user clusterd have users=*; buckets=*?)")
	case errors.As(err, &se):
		writeError(w, http.StatusBadGateway, se.Error())
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "body: "+err.Error())
		return false
	}
	return true
}

// users lists the object store's users, or one with {name}.
func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	cl, err := s.rgw()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	_, buckets := s.status.S3(r.Context())
	user := func(uid string) (api.S3User, error) {
		info, err := cl.User(r.Context(), uid)
		if err != nil {
			return api.S3User{}, err
		}
		u := api.S3User{Name: info.UserID, DisplayName: info.DisplayName, Suspended: info.Suspended != 0, Keys: len(info.Keys)}
		for _, b := range buckets {
			if b.Owner == uid {
				u.Buckets++
				u.Objects += b.Objects
				u.SizeBytes += b.SizeBytes
			}
		}
		return u, nil
	}
	if name := r.PathValue("name"); name != "" {
		if name == ownS3User || !validS3User.MatchString(name) {
			writeError(w, http.StatusNotFound, "no user "+strconv.Quote(name))
			return
		}
		u, err := user(name)
		if s3admin.IsStatus(err, http.StatusNotFound) {
			writeError(w, http.StatusNotFound, "no user "+strconv.Quote(name))
			return
		} else if err != nil {
			rgwError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, u)
		return
	}
	uids, err := cl.Users(r.Context())
	if err != nil {
		rgwError(w, err)
		return
	}
	l := api.List[api.S3User]{Items: []api.S3User{}}
	sort.Strings(uids)
	for _, uid := range uids {
		if uid == ownS3User {
			continue
		}
		u, err := user(uid)
		if err != nil {
			l.Errors = append(l.Errors, uid+": "+err.Error())
			continue
		}
		l.Items = append(l.Items, u)
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in api.S3UserCreate
	if !readBody(w, r, &in) {
		return
	}
	if !validS3User.MatchString(in.Name) || in.Name == ownS3User {
		writeError(w, http.StatusBadRequest, "name: lowercase letters, digits, '.', '_' or '-', at most 63")
		return
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Name
	}
	cl, err := s.rgw()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if _, err := cl.User(r.Context(), in.Name); err == nil {
		writeError(w, http.StatusConflict, "user "+strconv.Quote(in.Name)+" exists")
		return
	}
	info, err := cl.CreateUser(r.Context(), in.Name, in.DisplayName)
	if err == nil && len(info.Keys) == 0 {
		err = errors.New("the gateway created the user without a key")
	}
	s.audit(w, r, "s3 user create", in.Name, err)
	if err != nil {
		rgwError(w, err)
		return
	}
	k := info.Keys[0]
	w.Header().Set("Location", "/v1/s3/users/"+in.Name)
	writeJSON(w, http.StatusCreated, api.S3Credentials{User: in.Name, AccessKey: k.AccessKey, SecretKey: k.SecretKey, Endpoint: s.cfg.S3.Endpoint})
}

// deleteUser refuses while the user owns buckets: deleting data takes
// deleting those first.
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == ownS3User || !validS3User.MatchString(name) {
		writeError(w, http.StatusNotFound, "no user "+strconv.Quote(name))
		return
	}
	cl, err := s.rgw()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if _, err := cl.User(r.Context(), name); s3admin.IsStatus(err, http.StatusNotFound) {
		writeError(w, http.StatusNotFound, "no user "+strconv.Quote(name))
		return
	} else if err != nil {
		rgwError(w, err)
		return
	}
	_, buckets := s.status.S3(r.Context())
	var owned []string
	for _, b := range buckets {
		if b.Owner == name {
			owned = append(owned, b.Name)
		}
	}
	if len(owned) > 0 {
		writeError(w, http.StatusPreconditionFailed, fmt.Sprintf("%s owns buckets %v; delete those first", name, owned))
		return
	}
	err = cl.DeleteUser(r.Context(), name)
	s.audit(w, r, "s3 user delete", name, err)
	if err != nil {
		rgwError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rotateKeys gives the user a new key and removes its old ones: whatever
// used them stops working.
func (s *Server) rotateKeys(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == ownS3User || !validS3User.MatchString(name) {
		writeError(w, http.StatusNotFound, "no user "+strconv.Quote(name))
		return
	}
	cl, err := s.rgw()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	info, err := cl.User(r.Context(), name)
	if s3admin.IsStatus(err, http.StatusNotFound) {
		writeError(w, http.StatusNotFound, "no user "+strconv.Quote(name))
		return
	} else if err != nil {
		rgwError(w, err)
		return
	}
	old := map[string]bool{}
	for _, k := range info.Keys {
		old[k.AccessKey] = true
	}
	keys, err := cl.NewKey(r.Context(), name)
	var fresh *s3admin.Key
	if err == nil {
		i := slices.IndexFunc(keys, func(k s3admin.Key) bool { return k.User == name && !old[k.AccessKey] })
		if i < 0 {
			err = errors.New("the gateway returned no new key")
		} else {
			fresh = &keys[i]
		}
	}
	for ak := range old {
		if err != nil {
			break
		}
		err = cl.DeleteKey(r.Context(), name, ak)
	}
	s.audit(w, r, "s3 user rotate-keys", name, err)
	if err != nil {
		rgwError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.S3Credentials{User: name, AccessKey: fresh.AccessKey, SecretKey: fresh.SecretKey, Endpoint: s.cfg.S3.Endpoint})
}

func (s *Server) findBucket(w http.ResponseWriter, r *http.Request, name string) (api.S3Bucket, bool) {
	_, buckets := s.status.S3(r.Context())
	i := slices.IndexFunc(buckets, func(b api.S3Bucket) bool { return b.Name == name })
	if i < 0 {
		writeError(w, http.StatusNotFound, "no bucket "+strconv.Quote(name))
		return api.S3Bucket{}, false
	}
	return buckets[i], true
}

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request) {
	var in api.S3BucketCreate
	if !readBody(w, r, &in) {
		return
	}
	switch {
	case !validS3Bucket.MatchString(in.Name):
		writeError(w, http.StatusBadRequest, "name: 3 to 63 lowercase letters, digits, '.' or '-'")
		return
	case in.Owner == ownS3User || !validS3User.MatchString(in.Owner):
		writeError(w, http.StatusBadRequest, "owner: an existing user")
		return
	case in.QuotaBytes < 0:
		writeError(w, http.StatusBadRequest, "quota_bytes: 0 (none) or more")
		return
	}
	cl, err := s.rgw()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	owner, err := cl.User(r.Context(), in.Owner)
	if s3admin.IsStatus(err, http.StatusNotFound) {
		writeError(w, http.StatusBadRequest, "no user "+strconv.Quote(in.Owner))
		return
	} else if err != nil {
		rgwError(w, err)
		return
	}
	if len(owner.Keys) == 0 {
		writeError(w, http.StatusPreconditionFailed, in.Owner+" has no key to create the bucket with")
		return
	}
	err = cl.CreateBucket(r.Context(), s3admin.Credentials{AccessKey: owner.Keys[0].AccessKey, SecretKey: owner.Keys[0].SecretKey}, in.Name)
	if err == nil && in.QuotaBytes > 0 {
		err = cl.SetBucketQuota(r.Context(), in.Owner, in.Name, in.QuotaBytes)
	}
	s.audit(w, r, "s3 bucket create", in.Name, err)
	if err != nil {
		rgwError(w, err)
		return
	}
	b, ok := s.findBucket(w, r, in.Name)
	if !ok {
		return
	}
	w.Header().Set("Location", "/v1/s3/buckets/"+in.Name)
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) updateBucket(w http.ResponseWriter, r *http.Request) {
	var in api.S3BucketUpdate
	if !readBody(w, r, &in) {
		return
	}
	if in.QuotaBytes == nil || *in.QuotaBytes < 0 {
		writeError(w, http.StatusBadRequest, "quota_bytes: 0 (none) or more")
		return
	}
	b, ok := s.findBucket(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	cl, err := s.rgw()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	err = cl.SetBucketQuota(r.Context(), b.Owner, b.Name, *in.QuotaBytes)
	s.audit(w, r, "s3 bucket quota", b.Name, err)
	if err != nil {
		rgwError(w, err)
		return
	}
	if b, ok = s.findBucket(w, r, b.Name); ok {
		writeJSON(w, http.StatusOK, b)
	}
}

// deleteBucket refuses while the bucket holds objects, unless ?force=true.
func (s *Server) deleteBucket(w http.ResponseWriter, r *http.Request) {
	b, ok := s.findBucket(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	force := flag(r, "force")
	if b.Objects > 0 && !force {
		writeError(w, http.StatusPreconditionFailed, fmt.Sprintf("%s holds %d objects; force=true deletes them too", b.Name, b.Objects))
		return
	}
	cl, err := s.rgw()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	err = cl.DeleteBucket(r.Context(), b.Name, force)
	s.audit(w, r, "s3 bucket delete", b.Name, err)
	if err != nil {
		rgwError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
