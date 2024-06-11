package srclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/crxfoz/goavro/v2"
	"github.com/santhosh-tekuri/jsonschema/v5"
	"golang.org/x/sync/semaphore"
)

// ISchemaRegistryClient provides the
// definition of the operations that
// this Schema Registry client provides.
type ISchemaRegistryClient interface {
	GetGlobalCompatibilityLevel(ctx context.Context) (*CompatibilityLevel, error)
	GetCompatibilityLevel(ctx context.Context, subject string, defaultToGlobal bool) (*CompatibilityLevel, error)
	GetSubjects(ctx context.Context) ([]string, error)
	GetSubjectsIncludingDeleted(ctx context.Context) ([]string, error)
	GetSchema(ctx context.Context, schemaID int) (*Schema, error)
	GetLatestSchema(ctx context.Context, subject string) (*Schema, error)
	GetSchemaVersions(ctx context.Context, subject string) ([]int, error)
	GetSchemaByVersion(ctx context.Context, subject string, version int) (*Schema, error)
	CreateSchema(ctx context.Context, subject string, schema string, schemaType SchemaType, references ...Reference) (*Schema, error)
	LookupSchema(ctx context.Context, subject string, schema string, schemaType SchemaType, references ...Reference) (*Schema, error)
	ChangeSubjectCompatibilityLevel(ctx context.Context, subject string, compatibility CompatibilityLevel) (*CompatibilityLevel, error)
	DeleteSubject(ctx context.Context, subject string, permanent bool) error
	DeleteSubjectByVersion(ctx context.Context, subject string, version int, permanent bool) error
	SetCredentials(username string, password string)
	SetBearerToken(token TokenProvider)
	SetTimeout(timeout time.Duration)
	CachingEnabled(value bool)
	ResetCache()
	CodecCreationEnabled(value bool)
	IsSchemaCompatible(ctx context.Context, subject, schema, version string, schemaType SchemaType, references ...Reference) (bool, error)
}

// SchemaRegistryClient allows interactions with
// Schema Registry over HTTP. Applications using
// this client can retrieve data about schemas,
// which in turn can be used to serialize and
// deserialize data.
type SchemaRegistryClient struct {
	schemaRegistryURL        string
	credsLock                sync.RWMutex
	credentials              *credentials
	httpClient               *http.Client
	cachingEnabled           bool
	cachingEnabledLock       sync.RWMutex
	cacheLatest              bool
	cacheLatestLock          sync.RWMutex
	codecCreationEnabled     bool
	codecCreationEnabledLock sync.RWMutex
	idSchemaCache            map[int]*Schema
	idSchemaCacheLock        sync.RWMutex
	subjectSchemaCache       map[string]*Schema
	subjectSchemaCacheLock   sync.RWMutex
	sem                      *semaphore.Weighted
}

var _ ISchemaRegistryClient = new(SchemaRegistryClient)

type SchemaType string

const (
	Protobuf SchemaType = "PROTOBUF"
	Avro     SchemaType = "AVRO"
	Json     SchemaType = "JSON"
)

func (s SchemaType) String() string {
	// Avro is the default schemaType.
	// Returning "" omits schemaType from the schemaRequest JSON for compatibility with older API versions.
	if s == Avro {
		return ""
	}

	return string(s)
}

type CompatibilityLevel string

const (
	None               CompatibilityLevel = "NONE"
	Backward           CompatibilityLevel = "BACKWARD"
	BackwardTransitive CompatibilityLevel = "BACKWARD_TRANSITIVE"
	Forward            CompatibilityLevel = "FORWARD"
	ForwardTransitive  CompatibilityLevel = "FORWARD_TRANSITIVE"
	Full               CompatibilityLevel = "FULL"
	FullTransitive     CompatibilityLevel = "FULL_TRANSITIVE"
)

func (s CompatibilityLevel) String() string {
	return string(s)
}

// Schema references use the import statement of Protobuf and
// the $ref field of JSON Schema. They are defined by the name
// of the import or $ref and the associated subject in the registry.
type Reference struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Version int    `json:"version"`
}

// Schema is a data structure that holds all
// the relevant information about schemas.
type Schema struct {
	id         int
	schema     string
	schemaType *SchemaType
	version    int
	references []Reference
	codec      *goavro.Codec
	jsonSchema *jsonschema.Schema
}

// credentials can have either username AND password
// OR a bearerToken, it cannot have both forms of authentication
type credentials struct {
	// Username and Password for Schema Basic Auth
	username string
	password string

	// Bearer Authorization token
	bearerToken TokenProvider
}

type schemaRequest struct {
	Schema     string      `json:"schema"`
	SchemaType string      `json:"schemaType,omitempty"`
	References []Reference `json:"references,omitempty"`
}

type schemaResponse struct {
	Subject    string      `json:"subject"`
	Version    int         `json:"version"`
	Schema     string      `json:"schema"`
	SchemaType *SchemaType `json:"schemaType"`
	ID         int         `json:"id"`
	References []Reference `json:"references"`
}

type isCompatibleResponse struct {
	IsCompatible bool `json:"is_compatible"`
}

type configResponse struct {
	CompatibilityLevel CompatibilityLevel `json:"compatibilityLevel"`
}

type configChangeRequest struct {
	CompatibilityLevel CompatibilityLevel `json:"compatibility"`
}

type configChangeResponse configChangeRequest

const (
	schemaByID       = "/schemas/ids/%d"
	subjectBySubject = "/subjects/%s"
	subjectVersions  = "/subjects/%s/versions"
	subjectByVersion = "/subjects/%s/versions/%s"
	subjects         = "/subjects"
	config           = "/config"
	configBySubject  = "/config/%s"
	contentType      = "application/vnd.schemaregistry.v1+json"
)

// CreateSchemaRegistryClient creates a client that allows
// interactions with Schema Registry over HTTP. Applications
// using this client can retrieve data about schemas, which
// in turn can be used to serialize and deserialize records.
func CreateSchemaRegistryClient(schemaRegistryURL string) *SchemaRegistryClient {
	return CreateSchemaRegistryClientWithOptions(schemaRegistryURL, &http.Client{Timeout: 5 * time.Second}, 16)
}

// CreateSchemaRegistryClientWithOptions provides the ability to pass the http.Client to be used, as well as the semaphoreWeight for concurrent requests
func CreateSchemaRegistryClientWithOptions(schemaRegistryURL string, client *http.Client, semaphoreWeight int) *SchemaRegistryClient {
	return &SchemaRegistryClient{
		schemaRegistryURL:    schemaRegistryURL,
		httpClient:           client,
		cachingEnabled:       true,
		cacheLatest:          false,
		codecCreationEnabled: false,
		idSchemaCache:        make(map[int]*Schema),
		subjectSchemaCache:   make(map[string]*Schema),
		sem:                  semaphore.NewWeighted(int64(semaphoreWeight)),
	}
}

// ResetCache resets the schema caches to be able to get updated schemas.
func (client *SchemaRegistryClient) ResetCache() {

	client.idSchemaCacheLock.Lock()
	client.subjectSchemaCacheLock.Lock()
	client.idSchemaCache = make(map[int]*Schema)
	client.subjectSchemaCache = make(map[string]*Schema)
	client.idSchemaCacheLock.Unlock()
	client.subjectSchemaCacheLock.Unlock()

}

// GetSchema gets the schema associated with the given id.
func (client *SchemaRegistryClient) GetSchema(ctx context.Context, schemaID int) (*Schema, error) {

	if client.getCachingEnabled() {
		client.idSchemaCacheLock.RLock()
		cachedSchema := client.idSchemaCache[schemaID]
		client.idSchemaCacheLock.RUnlock()
		if cachedSchema != nil {
			return cachedSchema, nil
		}
	}

	resp, err := client.httpRequest(ctx, "GET", fmt.Sprintf(schemaByID, schemaID), nil)
	if err != nil {
		return nil, err
	}

	var schemaResp = new(schemaResponse)
	err = json.Unmarshal(resp, &schemaResp)
	if err != nil {
		return nil, err
	}
	var codec *goavro.Codec
	if client.getCodecCreationEnabled() {
		codec, err = goavro.NewCodec(schemaResp.Schema)
		if err != nil {
			return nil, err
		}
	}
	var schema = &Schema{
		id:         schemaID,
		schema:     schemaResp.Schema,
		version:    schemaResp.Version,
		schemaType: schemaResp.SchemaType,
		references: schemaResp.References,
		codec:      codec,
	}

	if client.getCachingEnabled() {
		client.idSchemaCacheLock.Lock()
		client.idSchemaCache[schemaID] = schema
		client.idSchemaCacheLock.Unlock()
	}

	return schema, nil
}

// GetLatestSchema gets the schema associated with the given subject.
// The schema returned contains the last version for that subject.
func (client *SchemaRegistryClient) GetLatestSchema(ctx context.Context, subject string) (*Schema, error) {
	return client.getVersion(ctx, subject, "latest")
}

// GetSchemaVersions returns a list of versions from a given subject.
func (client *SchemaRegistryClient) GetSchemaVersions(ctx context.Context, subject string) ([]int, error) {
	resp, err := client.httpRequest(ctx, "GET", fmt.Sprintf(subjectVersions, url.QueryEscape(subject)), nil)
	if err != nil {
		return nil, err
	}

	var versions = []int{}
	err = json.Unmarshal(resp, &versions)
	if err != nil {
		return nil, err
	}

	return versions, nil
}

// ChangeSubjectCompatibilityLevel changes the compatibility level of the subject.
func (client *SchemaRegistryClient) ChangeSubjectCompatibilityLevel(ctx context.Context, subject string, compatibility CompatibilityLevel) (*CompatibilityLevel, error) {
	configChangeReq := configChangeRequest{CompatibilityLevel: compatibility}
	configChangeReqBytes, err := json.Marshal(configChangeReq)
	if err != nil {
		return nil, err
	}
	payload := bytes.NewBuffer(configChangeReqBytes)

	resp, err := client.httpRequest(ctx, "PUT", fmt.Sprintf(configBySubject, url.QueryEscape(subject)), payload)
	if err != nil {
		return nil, err
	}

	var cfgChangeResp = new(configChangeResponse)
	err = json.Unmarshal(resp, &cfgChangeResp)
	if err != nil {
		return nil, err
	}

	return &cfgChangeResp.CompatibilityLevel, nil
}

// GetGlobalCompatibilityLevel returns the global compatibility level of the registry.
func (client *SchemaRegistryClient) GetGlobalCompatibilityLevel(ctx context.Context) (*CompatibilityLevel, error) {
	resp, err := client.httpRequest(ctx, "GET", config, nil)
	if err != nil {
		return nil, err
	}

	var configResponse = new(configResponse)
	err = json.Unmarshal(resp, &configResponse)
	if err != nil {
		return nil, err
	}

	return &configResponse.CompatibilityLevel, nil
}

// GetCompatibilityLevel returns the compatibility level of the subject.
// If defaultToGlobal is set to true and no compatibility level is set on the subject, the global compatibility level is returned.
func (client *SchemaRegistryClient) GetCompatibilityLevel(ctx context.Context, subject string, defaultToGlobal bool) (*CompatibilityLevel, error) {
	resp, err := client.httpRequest(ctx, "GET", fmt.Sprintf(configBySubject+"?defaultToGlobal=%t", url.QueryEscape(subject), defaultToGlobal), nil)
	if err != nil {
		return nil, err
	}

	var configResponse = new(configResponse)
	err = json.Unmarshal(resp, &configResponse)
	if err != nil {
		return nil, err
	}

	return &configResponse.CompatibilityLevel, nil
}

// GetSubjects returns a list of all subjects in the registry
func (client *SchemaRegistryClient) GetSubjects(ctx context.Context) ([]string, error) {
	resp, err := client.httpRequest(ctx, "GET", subjects, nil)
	if err != nil {
		return nil, err
	}
	var allSubjects = []string{}
	err = json.Unmarshal(resp, &allSubjects)
	if err != nil {
		return nil, err
	}
	return allSubjects, nil
}

// GetSubjectsIncludingDeleted returns a list of all subjects in the registry including those which have been soft deleted
func (client *SchemaRegistryClient) GetSubjectsIncludingDeleted(ctx context.Context) ([]string, error) {
	resp, err := client.httpRequest(ctx, "GET", subjects+"?deleted=true", nil)
	if err != nil {
		return nil, err
	}
	var allSubjects = []string{}
	err = json.Unmarshal(resp, &allSubjects)
	if err != nil {
		return nil, err
	}
	return allSubjects, nil
}

// GetSchemaByVersion gets the schema associated with the given subject.
// The schema returned contains the version specified as a parameter.
func (client *SchemaRegistryClient) GetSchemaByVersion(ctx context.Context, subject string, version int) (*Schema, error) {
	return client.getVersion(ctx, subject, strconv.Itoa(version))
}

// CreateSchema creates a new schema in Schema Registry and associates
// with the subject provided. It returns the newly created schema with
// all its associated information.
func (client *SchemaRegistryClient) CreateSchema(ctx context.Context,
	subject string, schema string,
	schemaType SchemaType, references ...Reference) (*Schema, error) {
	switch schemaType {
	case Avro, Json:
		compiledRegex := regexp.MustCompile(`\r?\n`)
		schema = compiledRegex.ReplaceAllString(schema, " ")
	case Protobuf:
		break
	default:
		return nil, fmt.Errorf("invalid schema type. valid values are Avro, Json, or Protobuf")
	}

	if references == nil {
		references = make([]Reference, 0)
	}

	schemaReq := schemaRequest{Schema: schema, SchemaType: schemaType.String(), References: references}
	schemaBytes, err := json.Marshal(schemaReq)
	if err != nil {
		return nil, err
	}
	payload := bytes.NewBuffer(schemaBytes)
	resp, err := client.httpRequest(ctx, "POST", fmt.Sprintf(subjectVersions, url.QueryEscape(subject)), payload)
	if err != nil {
		return nil, err
	}

	schemaResp := new(schemaResponse)
	err = json.Unmarshal(resp, &schemaResp)
	if err != nil {
		return nil, err
	}

	newSchema, err := client.GetSchema(ctx, schemaResp.ID)
	if err != nil {
		return nil, err
	}

	if client.getCachingEnabled() {

		// Update the subject-2-schema cache
		cacheKey := cacheKey(subject,
			strconv.Itoa(newSchema.version))
		client.subjectSchemaCacheLock.Lock()
		client.subjectSchemaCache[cacheKey] = newSchema
		client.subjectSchemaCacheLock.Unlock()

		// Update the id-2-schema cache
		client.idSchemaCacheLock.Lock()
		client.idSchemaCache[newSchema.id] = newSchema
		client.idSchemaCacheLock.Unlock()

	}

	return newSchema, nil
}

// LookupSchema looks up the schema by subject and schema string. If it finds the schema it returns it with all its associated information.
func (client *SchemaRegistryClient) LookupSchema(ctx context.Context, subject string, schema string, schemaType SchemaType, references ...Reference) (*Schema, error) {
	switch schemaType {
	case Avro, Json:
		compiledRegex := regexp.MustCompile(`\r?\n`)
		schema = compiledRegex.ReplaceAllString(schema, " ")
	case Protobuf:
		break
	default:
		return nil, fmt.Errorf("invalid schema type. valid values are Avro, Json, or Protobuf")
	}

	if references == nil {
		references = make([]Reference, 0)
	}

	schemaReq := schemaRequest{Schema: schema, SchemaType: schemaType.String(), References: references}
	schemaBytes, err := json.Marshal(schemaReq)
	if err != nil {
		return nil, err
	}
	payload := bytes.NewBuffer(schemaBytes)
	resp, err := client.httpRequest(ctx, "POST", fmt.Sprintf(subjectBySubject, url.QueryEscape(subject)), payload)
	if err != nil {
		return nil, err
	}

	schemaResp := new(schemaResponse)
	err = json.Unmarshal(resp, &schemaResp)
	if err != nil {
		return nil, err
	}

	var codec *goavro.Codec
	if client.getCodecCreationEnabled() && schemaType == Avro {
		codec, err = goavro.NewCodec(schemaResp.Schema)
		if err != nil {
			return nil, err
		}
	}
	var gotSchema = &Schema{
		id:         schemaResp.ID,
		schema:     schemaResp.Schema,
		schemaType: schemaResp.SchemaType,
		version:    schemaResp.Version,
		references: schemaResp.References,
		codec:      codec,
	}

	if client.getCachingEnabled() {

		// Update the subject-2-schema cache
		cacheKey := cacheKey(subject,
			strconv.Itoa(gotSchema.version))
		client.subjectSchemaCacheLock.Lock()
		client.subjectSchemaCache[cacheKey] = gotSchema
		client.subjectSchemaCacheLock.Unlock()

		// Update the id-2-schema cache
		client.idSchemaCacheLock.Lock()
		client.idSchemaCache[gotSchema.id] = gotSchema
		client.idSchemaCacheLock.Unlock()

	}

	return gotSchema, nil
}

// IsSchemaCompatible checks if the given schema is compatible with the given subject and version
// valid versions are versionID and "latest"
func (client *SchemaRegistryClient) IsSchemaCompatible(ctx context.Context, subject, schema, version string, schemaType SchemaType, references ...Reference) (bool, error) {
	if references == nil {
		references = make([]Reference, 0)
	}

	schemaReq := schemaRequest{Schema: schema, SchemaType: schemaType.String(), References: references}
	schemaReqBytes, err := json.Marshal(schemaReq)
	if err != nil {
		return false, err
	}
	payload := bytes.NewBuffer(schemaReqBytes)

	url := fmt.Sprintf("/compatibility/subjects/%s/versions/%s", subject, version)
	resp, err := client.httpRequest(ctx, "POST", url, payload)
	if err != nil {
		return false, err
	}

	compatibilityResponse := new(isCompatibleResponse)
	err = json.Unmarshal(resp, compatibilityResponse)
	if err != nil {
		return false, err
	}

	return compatibilityResponse.IsCompatible, nil
}

// DeleteSubject deletes
func (client *SchemaRegistryClient) DeleteSubject(ctx context.Context, subject string, permanent bool) error {
	uri := "/subjects/" + subject
	_, err := client.httpRequest(ctx, "DELETE", uri, nil)
	if err != nil || !permanent {
		return err
	}

	uri += "?permanent=true"
	_, err = client.httpRequest(ctx, "DELETE", uri, nil)
	return err
}

// DeleteSubjectByVersion deletes the version of the scheme
func (client *SchemaRegistryClient) DeleteSubjectByVersion(ctx context.Context, subject string, version int, permanent bool) error {
	uri := fmt.Sprintf(subjectByVersion, subject, strconv.Itoa(version))
	_, err := client.httpRequest(ctx, "DELETE", uri, nil)
	if err != nil || !permanent {
		return err
	}

	uri += "?permanent=true"
	_, err = client.httpRequest(ctx, "DELETE", uri, nil)
	return err
}

// SetCredentials allows users to set credentials to be
// used with Schema Registry, for scenarios when Schema
// Registry has authentication enabled.
func (client *SchemaRegistryClient) SetCredentials(username string, password string) {
	client.credsLock.Lock()
	defer client.credsLock.Unlock()

	if len(username) > 0 && len(password) > 0 {
		credentials := credentials{username: username, password: password, bearerToken: nil}
		client.credentials = &credentials
	}
}

// SetBearerToken allows users to add a Bearer Token
// http header with calls to Schema Registry
// The BearerToken will override Schema Registry credentials
func (client *SchemaRegistryClient) SetBearerToken(token TokenProvider) {
	client.credsLock.Lock()
	defer client.credsLock.Unlock()

	if token != nil {
		credentials := credentials{username: "", password: "", bearerToken: token}
		client.credentials = &credentials
	}
}

// SetTimeout allows the client to be reconfigured about
// how much time internal HTTP requests will take until
// they timeout. FYI, It defaults to five seconds.
func (client *SchemaRegistryClient) SetTimeout(timeout time.Duration) {
	client.httpClient.Timeout = timeout
}

// CachingEnabled allows the client to cache any values
// that have been returned, which may speed up performance
// if these values rarely changes.
func (client *SchemaRegistryClient) CachingEnabled(value bool) {
	client.cachingEnabledLock.Lock()
	defer client.cachingEnabledLock.Unlock()
	client.cachingEnabled = value
}

// CacheLatest controls if schemas with the latest as a version is cached.
func (client *SchemaRegistryClient) CacheLatest(value bool) {
	client.cacheLatestLock.Lock()
	defer client.cacheLatestLock.Unlock()
	client.cacheLatest = value
}

// CodecCreationEnabled allows the application to enable/disable
// the automatic creation of codec's when schemas are returned.
func (client *SchemaRegistryClient) CodecCreationEnabled(value bool) {
	client.codecCreationEnabledLock.Lock()
	defer client.codecCreationEnabledLock.Unlock()
	client.codecCreationEnabled = value
}

func (client *SchemaRegistryClient) getVersion(ctx context.Context, subject string, version string) (*Schema, error) {

	if client.getCachingEnabled() {
		if version != "latest" || (version == "latest" && client.getCacheLatest()) {
			cacheKey := cacheKey(subject, version)
			client.subjectSchemaCacheLock.RLock()
			cachedResult := client.subjectSchemaCache[cacheKey]
			client.subjectSchemaCacheLock.RUnlock()
			if cachedResult != nil {
				return cachedResult, nil
			}
		}
	}

	resp, err := client.httpRequest(ctx, "GET", fmt.Sprintf(subjectByVersion, url.QueryEscape(subject), version), nil)
	if err != nil {
		return nil, err
	}

	schemaResp := new(schemaResponse)
	err = json.Unmarshal(resp, &schemaResp)
	if err != nil {
		return nil, err
	}
	var codec *goavro.Codec
	if client.getCodecCreationEnabled() {
		codec, err = goavro.NewCodec(schemaResp.Schema)
		if err != nil {
			return nil, err
		}
	}
	var schema = &Schema{
		id:         schemaResp.ID,
		schema:     schemaResp.Schema,
		schemaType: schemaResp.SchemaType,
		version:    schemaResp.Version,
		references: schemaResp.References,
		codec:      codec,
	}

	if client.getCachingEnabled() {
		if version != "latest" || (version == "latest" && client.getCacheLatest()) {
			// Update the subject-2-schema cache
			cacheKey := cacheKey(subject, version)
			client.subjectSchemaCacheLock.Lock()
			client.subjectSchemaCache[cacheKey] = schema
			client.subjectSchemaCacheLock.Unlock()
		}

		// Update the id-2-schema cache
		client.idSchemaCacheLock.Lock()
		client.idSchemaCache[schema.id] = schema
		client.idSchemaCacheLock.Unlock()

	}

	return schema, nil
}

func (client *SchemaRegistryClient) httpRequest(ctx context.Context, method, uri string, payload io.Reader) ([]byte, error) {

	url := fmt.Sprintf("%s%s", client.schemaRegistryURL, uri)
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return nil, err
	}

	client.credsLock.RLock()
	if client.credentials != nil {
		if len(client.credentials.username) > 0 && len(client.credentials.password) > 0 {
			req.SetBasicAuth(client.credentials.username, client.credentials.password)
		} else if client.credentials.bearerToken != nil {
			token, err := client.credentials.bearerToken.ObtainToken(ctx)
			if err != nil {
				return nil, err
			}

			req.Header.Add("Authorization", "Bearer "+token)
		}
	}
	client.credsLock.RUnlock()

	req.Header.Set("Content-Type", contentType)

	client.sem.Acquire(context.Background(), 1)
	defer client.sem.Release(1)
	resp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, createError(resp)
	}

	return ioutil.ReadAll(resp.Body)
}

func (client *SchemaRegistryClient) getCachingEnabled() bool {
	client.cachingEnabledLock.RLock()
	defer client.cachingEnabledLock.RUnlock()
	return client.cachingEnabled
}

func (client *SchemaRegistryClient) getCacheLatest() bool {
	client.cacheLatestLock.RLock()
	defer client.cacheLatestLock.RUnlock()
	return client.cacheLatest
}

func (client *SchemaRegistryClient) getCodecCreationEnabled() bool {
	client.codecCreationEnabledLock.RLock()
	defer client.codecCreationEnabledLock.RUnlock()
	return client.codecCreationEnabled
}

// NewSchema instantiates a new Schema struct.
func NewSchema(
	id int,
	schema string,
	schemaType SchemaType,
	version int,
	references []Reference,
	codec *goavro.Codec,
	jsonSchema *jsonschema.Schema,
) (*Schema, error) {
	if schema == "" {
		return nil, errors.New("schema cannot be nil")
	}
	return &Schema{
		id:         id,
		schema:     schema,
		schemaType: &schemaType,
		version:    version,
		references: references,
		codec:      codec,
		jsonSchema: jsonSchema,
	}, nil
}

// ID ensures access to ID
func (schema *Schema) ID() int {
	return schema.id
}

// Schema ensures access to Schema
func (schema *Schema) Schema() string {
	return schema.schema
}

// SchemaType ensures access to SchemaType
func (schema *Schema) SchemaType() *SchemaType {
	return schema.schemaType
}

// Version ensures access to Version
func (schema *Schema) Version() int {
	return schema.version
}

// References ensures access to References
func (schema *Schema) References() []Reference {
	return schema.references
}

// Codec ensures access to Codec
// Will try to initialize a new one if it hasn't been initialized before
// Will return nil if it can't initialize a codec from the schema
func (schema *Schema) Codec() *goavro.Codec {
	if schema.codec == nil {
		codec, err := goavro.NewCodec(schema.Schema())
		if err == nil {
			schema.codec = codec
		}
	}
	return schema.codec
}

// JsonSchema ensures access to JsonSchema
// Will try to initialize a new one if it hasn't been initialized before
// Will return nil if it can't initialize a json schema from the schema
func (schema *Schema) JsonSchema() *jsonschema.Schema {
	if schema.jsonSchema == nil {
		jsonSchema, err := jsonschema.CompileString("schema.json", schema.Schema())
		if err == nil {
			schema.jsonSchema = jsonSchema
		}
	}
	return schema.jsonSchema
}

func cacheKey(subject string, version string) string {
	return fmt.Sprintf("%s-%s", subject, version)
}

// Error implements error, encodes HTTP errors from Schema Registry.
type Error struct {
	Code    int    `json:"error_code"`
	Message string `json:"message"`
	str     *bytes.Buffer
}

func (e Error) Error() string {
	return e.str.String()
}

func createError(resp *http.Response) error {
	err := Error{str: bytes.NewBuffer(make([]byte, 0))}
	decoder := json.NewDecoder(io.TeeReader(resp.Body, err.str))
	marshalErr := decoder.Decode(&err)
	if marshalErr != nil {
		return fmt.Errorf("%s", resp.Status)
	}

	return err
}
