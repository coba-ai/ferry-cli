// Package creds is the credential store: one 0600 file, one profile per
// named environment of use, and at most one credential per credential class.
//
// Three properties are load-bearing and each has an acceptance criterion:
//
//   - the file is written atomically at 0600 and a file wider than 0600 is
//     refused on read (AC7, C12);
//   - a token is classified by its shape alone, with no I/O, and a command
//     that needs a class the profile lacks is refused locally (AC8, C15);
//   - a credential is bound to the API URL it was verified against and is not
//     handed to any other (AC9, C7).
//
// The third is the one that matters most: FERRY has two credential classes
// and no way to tell a client which deployment a token came from, so the only
// thing standing between a sandbox key and a live host is this binding.
package creds

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kurenn/ferry-cli/internal/fsx"
)

// Schema is the version written into the file.
const Schema = 1

// FileMode is the mode the credentials file is written with, and the widest
// mode it may be read at.
const FileMode fs.FileMode = 0o600

// DefaultProfile is the profile used when none is named.
const DefaultProfile = "default"

// Class is a FERRY credential class. The API decides per endpoint which class
// it accepts (PLAN §1.2.1) and answers 403 WRONG_TOKEN_CLASS for the other,
// so the CLI decides the same thing locally before sending.
type Class string

const (
	ClassAPIKey Class = "api_key"
	ClassPAT    Class = "pat"
)

// Environment is the environment an API key belongs to. A PAT has none.
type Environment string

const (
	EnvSandbox Environment = "sandbox"
	EnvLive    Environment = "live"
)

// Token shapes, from PLAN §1.2.2 (lib/ferry/tokens.rb:57-58).
//
// The anchors are \A and \z, not ^ and $: in Go as in Ruby the line anchors
// would accept a valid token followed by a newline and anything after it, and
// this value is about to become an Authorization header.
var (
	apiKeyRE = regexp.MustCompile(`\Aferry_sk_(sandbox|live)_[0-9A-Za-z]{43}\z`)
	patRE    = regexp.MustCompile(`\Aferry_pat_[0-9A-Za-z]{43}\z`)
)

// Requirement is the credential class an endpoint needs (PLAN §5.2).
type Requirement int

const (
	// RequireAPIKey is transfers and commands.
	RequireAPIKey Requirement = iota
	// RequirePAT is the api_keys endpoints.
	RequirePAT
	// RequireEither is corridors and GET /v1/me.
	RequireEither
)

func (r Requirement) String() string {
	switch r {
	case RequireAPIKey:
		return "api_key"
	case RequirePAT:
		return "personal_access_token"
	case RequireEither:
		return "api_key or personal_access_token"
	}
	return "unknown"
}

// Errors the CLI branches on.
var (
	// ErrPermissionsTooWide is AC7: a credentials file readable by anybody
	// but its owner is refused rather than read.
	ErrPermissionsTooWide = errors.New("creds: credentials file permissions are wider than 0600")
	// ErrNoCredentialOfClass is AC8/C15: the profile has no credential of the
	// class the endpoint needs.
	ErrNoCredentialOfClass = errors.New("creds: no credential of the required class")
	// ErrAPIURLMismatch is AC9/C7: the credential was verified against a
	// different API URL.
	ErrAPIURLMismatch = errors.New("creds: credential is bound to a different API URL")
	// ErrUnknownTokenShape is a token that matches neither class regex.
	ErrUnknownTokenShape = errors.New("creds: token matches no known credential shape")
	// ErrNoProfile is a profile that does not exist.
	ErrNoProfile = errors.New("creds: no such profile")
	// ErrNoAPIURL is AC9: a profile cannot be created without an API URL.
	ErrNoAPIURL = errors.New("creds: a profile requires an api_url")
	// ErrBadAPIURL is a URL that is not a plain absolute http(s) endpoint.
	ErrBadAPIURL = errors.New("creds: api_url must be an absolute http or https URL with no user info, query or fragment")
	// ErrSchemaUnsupported is a credentials file from a future version.
	ErrSchemaUnsupported = errors.New("creds: unsupported credentials schema")
)

// Credential is one stored token and what is known about it.
//
// Environment is a pointer because null is meaningful: a PAT belongs to no
// environment (PLAN §5.2), and decoding that to the empty string would make
// "unknown" and "none" the same value.
type Credential struct {
	Token       string       `json:"token"`
	TokenPrefix string       `json:"token_prefix"`
	Environment *Environment `json:"environment"`
	PrincipalID string       `json:"principal_id"`
	StoredAt    time.Time    `json:"stored_at"`
}

// Last4 is the tail of the token, for a render that identifies a credential
// without printing it (AC36).
func (c Credential) Last4() string {
	if len(c.Token) < 4 {
		return ""
	}
	return c.Token[len(c.Token)-4:]
}

// Display is the only form of a credential that may be printed: the prefix
// and the last four characters, never the token.
func (c Credential) Display() string {
	if c.Token == "" {
		return ""
	}
	return c.TokenPrefix + "…" + c.Last4()
}

// Profile is one API URL and the credentials verified against it.
type Profile struct {
	APIURL string      `json:"api_url"`
	APIKey *Credential `json:"api_key,omitempty"`
	PAT    *Credential `json:"pat,omitempty"`
}

// File is the whole credentials document.
type File struct {
	SchemaVersion int                `json:"schema"`
	Profiles      map[string]Profile `json:"profiles"`
}

// NewFile returns an empty document.
func NewFile() *File {
	return &File{SchemaVersion: Schema, Profiles: map[string]Profile{}}
}

// Classify decides a token's class from its shape alone.
//
// It performs no I/O and consults nothing but the two regexes in PLAN
// §1.2.2, which is what lets the CLI refuse a wrong-class request before a
// byte leaves (AC8).
func Classify(token string) (Class, error) {
	switch {
	case apiKeyRE.MatchString(token):
		return ClassAPIKey, nil
	case patRE.MatchString(token):
		return ClassPAT, nil
	default:
		return "", ErrUnknownTokenShape
	}
}

// EnvironmentOf returns the environment an API key names, or nil for a PAT.
func EnvironmentOf(token string) (*Environment, error) {
	if m := apiKeyRE.FindStringSubmatch(token); m != nil {
		env := Environment(m[1])
		return &env, nil
	}
	if patRE.MatchString(token) {
		return nil, nil
	}
	return nil, ErrUnknownTokenShape
}

// Prefix is the identifying, non-secret head of a token: everything up to and
// including the last underscore, plus the first eight characters of the
// secret. It is what PLAN §5.2 stores as token_prefix and what C19 compares
// on a resume.
func Prefix(token string) string {
	i := strings.LastIndex(token, "_")
	if i < 0 {
		return ""
	}
	secret := token[i+1:]
	if len(secret) > 8 {
		secret = secret[:8]
	}
	return token[:i+1] + secret
}

// NormalizeAPIURL validates an API URL and returns its canonical form.
//
// Canonical means: scheme and host lowercased, a default port dropped, and a
// trailing slash removed from the path. Those three differences are the same
// endpoint under RFC 3986, and treating them as different would refuse a
// caller who typed the URL with a trailing slash. Everything else — user
// info, a query, a fragment, a non-http scheme, a relative URL — is refused,
// because each of them is a way to make a credential travel somewhere its
// owner did not read.
func NormalizeAPIURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", ErrNoAPIURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrBadAPIURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: scheme %q", ErrBadAPIURL, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: no host in %q", ErrBadAPIURL, raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: user info present", ErrBadAPIURL)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("%w: query present", ErrBadAPIURL)
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("%w: fragment present", ErrBadAPIURL)
	}

	host := strings.ToLower(u.Host)
	host = strings.TrimSuffix(host, ":443")
	if scheme == "http" {
		host = strings.TrimSuffix(host, ":80")
	}

	path := strings.TrimSuffix(u.EscapedPath(), "/")
	return scheme + "://" + host + path, nil
}

// SameAPIURL reports whether two URLs name the same endpoint.
func SameAPIURL(a, b string) bool {
	na, errA := NormalizeAPIURL(a)
	nb, errB := NormalizeAPIURL(b)
	if errA != nil || errB != nil {
		// An unparseable URL is compared literally rather than treated as
		// equal to anything.
		return errA == nil && errB == nil && a == b
	}
	return na == nb
}

// Put stores a token on a profile, classifying it by shape.
//
// It performs no I/O: it mutates the in-memory document, which the caller
// persists with Store.Save. apiURL is required, so a profile without one
// cannot be created (AC9); passing a different apiURL for an existing profile
// is a rebind and is refused, because the credentials already on the profile
// were verified somewhere else.
func (f *File) Put(profileName, apiURL string, cred Credential) error {
	normalized, err := NormalizeAPIURL(apiURL)
	if err != nil {
		return err
	}
	class, err := Classify(cred.Token)
	if err != nil {
		return err
	}
	env, err := EnvironmentOf(cred.Token)
	if err != nil {
		return err
	}
	if profileName == "" {
		profileName = DefaultProfile
	}
	if f.Profiles == nil {
		f.Profiles = map[string]Profile{}
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = Schema
	}

	p, existing := f.Profiles[profileName]
	if existing && p.APIURL != "" && !SameAPIURL(p.APIURL, normalized) {
		return fmt.Errorf("%w: profile %q is bound to %s, not %s",
			ErrAPIURLMismatch, profileName, p.APIURL, normalized)
	}
	p.APIURL = normalized

	cred.TokenPrefix = Prefix(cred.Token)
	cred.Environment = env
	if cred.StoredAt.IsZero() {
		cred.StoredAt = time.Now().UTC()
	} else {
		cred.StoredAt = cred.StoredAt.UTC()
	}

	switch class {
	case ClassAPIKey:
		p.APIKey = &cred
	case ClassPAT:
		p.PAT = &cred
	}
	f.Profiles[profileName] = p
	return nil
}

// Remove deletes the named class from a profile. A nil class removes both.
func (f *File) Remove(profileName string, class *Class) error {
	if profileName == "" {
		profileName = DefaultProfile
	}
	p, ok := f.Profiles[profileName]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoProfile, profileName)
	}
	switch {
	case class == nil:
		delete(f.Profiles, profileName)
	case *class == ClassAPIKey:
		p.APIKey = nil
		f.Profiles[profileName] = p
	case *class == ClassPAT:
		p.PAT = nil
		f.Profiles[profileName] = p
	}
	return nil
}

// Match is what For returns: the credential to send and what it is.
type Match struct {
	Credential Credential
	Class      Class
	Profile    string
	APIURL     string
}

// For returns the credential a requirement names, bound to apiURL.
//
// apiURL is the URL this invocation intends to call. An empty value means
// "whatever the profile is bound to" and is how a command with no --api flag
// asks. A non-empty value that names a different endpoint returns
// ErrAPIURLMismatch and a zero Match — the token is not returned at all,
// because returning it and expecting the caller not to use it is not a
// control (AC9, C7).
func (f *File) For(profileName string, req Requirement, apiURL string) (Match, error) {
	if profileName == "" {
		profileName = DefaultProfile
	}
	p, ok := f.Profiles[profileName]
	if !ok {
		return Match{}, fmt.Errorf("%w: %s", ErrNoProfile, profileName)
	}

	if apiURL != "" && !SameAPIURL(p.APIURL, apiURL) {
		return Match{}, fmt.Errorf("%w: profile %q is bound to %s, this invocation asks for %s",
			ErrAPIURLMismatch, profileName, p.APIURL, apiURL)
	}

	pick := func(c *Credential, class Class) (Match, bool) {
		if c == nil || c.Token == "" {
			return Match{}, false
		}
		return Match{Credential: *c, Class: class, Profile: profileName, APIURL: p.APIURL}, true
	}

	switch req {
	case RequireAPIKey:
		if m, ok := pick(p.APIKey, ClassAPIKey); ok {
			return m, nil
		}
	case RequirePAT:
		if m, ok := pick(p.PAT, ClassPAT); ok {
			return m, nil
		}
	case RequireEither:
		// An API key first: it is the class the money endpoints need, so a
		// profile holding both is most likely being used as a key profile.
		if m, ok := pick(p.APIKey, ClassAPIKey); ok {
			return m, nil
		}
		if m, ok := pick(p.PAT, ClassPAT); ok {
			return m, nil
		}
	}
	return Match{}, fmt.Errorf("%w: profile %q holds no %s", ErrNoCredentialOfClass, profileName, req)
}

// Store reads and writes the credentials file.
type Store struct {
	fs   fsx.FS
	path string
}

// NewStore returns a store over path.
func NewStore(filesystem fsx.FS, path string) *Store {
	if filesystem == nil {
		filesystem = fsx.OS()
	}
	return &Store{fs: filesystem, path: path}
}

// Path is the file the store reads and writes.
func (s *Store) Path() string { return s.path }

// Load reads the credentials file.
//
// A file whose mode grants any bit to group or other is refused with
// ErrPermissionsTooWide and is not read (AC7): by the time the CLI can see
// the contents the token has already been readable by whoever else could open
// the file, and reading it would only spread it further.
//
// A missing file is an empty document, not an error: the first `auth login`
// has nothing to read.
func (s *Store) Load() (*File, error) {
	info, err := s.fs.Stat(s.path)
	if err != nil {
		if fsx.ErrNotFound(err) {
			return NewFile(), nil
		}
		return nil, fmt.Errorf("creds: stat %s: %w", s.path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is %#o; run chmod 600 %s", ErrPermissionsTooWide, s.path, perm, s.path)
	}

	raw, err := s.fs.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("creds: read %s: %w", s.path, err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("creds: parse %s: %w", s.path, err)
	}
	if f.SchemaVersion > Schema {
		return nil, fmt.Errorf("%w: %s is schema %d, this binary understands %d",
			ErrSchemaUnsupported, s.path, f.SchemaVersion, Schema)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]Profile{}
	}
	return &f, nil
}

// Save writes the document atomically at 0600 (AC7, C12).
func (s *Store) Save(f *File) error {
	if f.SchemaVersion == 0 {
		f.SchemaVersion = Schema
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("creds: encode: %w", err)
	}
	data = append(data, '\n')
	if err := fsx.WriteFileAtomic(s.fs, s.path, data, FileMode); err != nil {
		return fmt.Errorf("creds: write %s: %w", s.path, err)
	}
	return nil
}
