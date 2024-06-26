package srclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"time"

	"github.com/crxfoz/goavro/v2"
)

// Compile-time interface check
var _ ISchemaRegistryClient = new(MockSchemaRegistryClient)

// Currently unexported to not pollute the interface
var (
	errInvalidSchemaType       = errors.New("invalid schema type. valid values are Avro, Json, or Protobuf")
	errSchemaAlreadyRegistered = errors.New("schema already registered")
	errSchemaNotFound          = errors.New("schema not found")
	errSubjectNotFound         = errors.New("subject not found")
	errNotImplemented          = errors.New("not implemented")
)

// MockSchemaRegistryClient represents an in-memory SchemaRegistryClient for testing purposes.
type MockSchemaRegistryClient struct {
	// schemaRegistryURL is used to form errors
	schemaRegistryURL string

	// schemaVersions is a map of subject to a map of versions to the actual schema
	schemaVersions map[string]map[int]*Schema

	// schemaIDs is a map of schema ID to the actual schema
	schemaIDs map[int]*Schema

	// idCounter is used to generate unique IDs for each schema
	idCounter int
}

// CreateMockSchemaRegistryClient initializes a MockSchemaRegistryClient
func CreateMockSchemaRegistryClient(mockURL string) *MockSchemaRegistryClient {
	mockClient := &MockSchemaRegistryClient{
		schemaRegistryURL: mockURL,
		schemaVersions:    map[string]map[int]*Schema{},
		schemaIDs:         map[int]*Schema{},
	}

	return mockClient
}

// avroRegex is used to remove whitespace from the schema string
var avroRegex = regexp.MustCompile(`\r?\n`)

// CreateSchema generates a new schema with the given details, references are unused
func (mck *MockSchemaRegistryClient) CreateSchema(ctx context.Context, subject string, schema string, schemaType SchemaType, _ ...Reference) (*Schema, error) {
	mck.idCounter++
	return mck.SetSchema(ctx, mck.idCounter, subject, schema, schemaType, -1)
}

// SetSchema overwrites a schema with the given id. Allows you to set a schema with a specific ID for testing purposes.
// Sets the ID counter to the given id if it is greater than the current counter. Version
// is used to set the version of the schema. If version is -1, the version will be set to the next available version.
func (mck *MockSchemaRegistryClient) SetSchema(_ context.Context, id int, subject string, schema string, schemaType SchemaType, version int) (*Schema, error) {
	if id > mck.idCounter {
		mck.idCounter = id
	}

	switch schemaType {
	case Avro, Json:
		schema = avroRegex.ReplaceAllString(schema, " ")
	case Protobuf:
		break
	default:
		return nil, errInvalidSchemaType
	}

	resultFromSchemaCache, ok := mck.schemaVersions[subject]
	if !ok {
		return mck.generateVersion(id, subject, schema, schemaType, version)
	}

	// Verify if it's not the same schema as an existing version
	for _, existing := range resultFromSchemaCache {
		if existing.schema == schema {
			posErr := url.Error{
				Op:  "POST",
				URL: fmt.Sprintf("%s/subjects/%s/versions", mck.schemaRegistryURL, subject),
				Err: errSchemaAlreadyRegistered,
			}
			return nil, &posErr
		}
	}

	return mck.generateVersion(id, subject, schema, schemaType, version)
}

// GetSchema Returns a Schema for the given ID
func (mck *MockSchemaRegistryClient) GetSchema(_ context.Context, schemaID int) (*Schema, error) {
	thisSchema, ok := mck.schemaIDs[schemaID]
	if !ok {
		posErr := url.Error{
			Op:  "GET",
			URL: fmt.Sprintf("%s/schemas/ids/%d", mck.schemaRegistryURL, schemaID),
			Err: errSchemaNotFound,
		}

		return nil, &posErr
	}
	return thisSchema, nil
}

// GetLatestSchema Returns the highest ordinal version of a Schema for a given `concrete subject`
func (mck *MockSchemaRegistryClient) GetLatestSchema(ctx context.Context, subject string) (*Schema, error) {
	// Error is never returned
	versions, _ := mck.GetSchemaVersions(ctx, subject)
	if len(versions) == 0 {
		return nil, errSchemaNotFound
	}

	latestVersion := versions[len(versions)-1]

	// This can't realistically throw an error
	thisSchema, _ := mck.GetSchemaByVersion(ctx, subject, latestVersion)

	return thisSchema, nil
}

// GetSchemaVersions Returns the array of versions this subject has previously registered
func (mck *MockSchemaRegistryClient) GetSchemaVersions(_ context.Context, subject string) ([]int, error) {
	versions := mck.allVersions(subject)
	return versions, nil
}

// GetSchemaByVersion Returns the given Schema according to the passed in subject and version number
func (mck *MockSchemaRegistryClient) GetSchemaByVersion(_ context.Context, subject string, version int) (*Schema, error) {
	var schema *Schema
	schemaVersionMap, ok := mck.schemaVersions[subject]
	if !ok {
		posErr := url.Error{
			Op:  "GET",
			URL: mck.schemaRegistryURL + fmt.Sprintf("/subjects/%s/versions/%d", subject, version),
			Err: errSubjectNotFound,
		}
		return nil, &posErr
	}
	for id, schemaL := range schemaVersionMap {
		if id == version {
			schema = schemaL
		}
	}

	if schema == nil {
		posErr := url.Error{
			Op:  "GET",
			URL: mck.schemaRegistryURL + fmt.Sprintf("/subjects/%s/versions/%d", subject, version),
			Err: errSchemaNotFound,
		}
		return nil, &posErr
	}

	return schema, nil
}

// GetSubjects Returns all registered subjects
func (mck *MockSchemaRegistryClient) GetSubjects(_ context.Context) ([]string, error) {
	var allSubjects []string

	for subject := range mck.schemaVersions {
		allSubjects = append(allSubjects, subject)
	}

	return allSubjects, nil
}

// GetSubjectsIncludingDeleted is not implemented and returns an error
func (mck *MockSchemaRegistryClient) GetSubjectsIncludingDeleted(_ context.Context) ([]string, error) {
	return nil, errNotImplemented
}

// DeleteSubject removes given subject from the cache
func (mck *MockSchemaRegistryClient) DeleteSubject(_ context.Context, subject string, _ bool) error {
	delete(mck.schemaVersions, subject)
	return nil
}

// DeleteSubjectByVersion removes given subject's version from cache
func (mck *MockSchemaRegistryClient) DeleteSubjectByVersion(_ context.Context, subject string, version int, _ bool) error {
	_, ok := mck.schemaVersions[subject]
	if !ok {
		posErr := url.Error{
			Op:  "DELETE",
			URL: fmt.Sprintf("%s/subjects/%s/versions/%d", mck.schemaRegistryURL, subject, version),
			Err: errSubjectNotFound,
		}
		return &posErr
	}

	for schemaVersion := range mck.schemaVersions[subject] {
		if schemaVersion == version {
			delete(mck.schemaVersions[subject], schemaVersion)
			return nil
		}
	}

	posErr := url.Error{
		Op:  "GET",
		URL: fmt.Sprintf("%s/subjects/%s/versions/%d", mck.schemaRegistryURL, subject, version),
		Err: errSchemaNotFound,
	}
	return &posErr
}

// ChangeSubjectCompatibilityLevel is not implemented
func (mck *MockSchemaRegistryClient) ChangeSubjectCompatibilityLevel(context.Context, string, CompatibilityLevel) (*CompatibilityLevel, error) {
	return nil, errNotImplemented
}

// GetGlobalCompatibilityLevel is not implemented
func (mck *MockSchemaRegistryClient) GetGlobalCompatibilityLevel(_ context.Context) (*CompatibilityLevel, error) {
	return nil, errNotImplemented
}

// GetCompatibilityLevel is not implemented
func (mck *MockSchemaRegistryClient) GetCompatibilityLevel(context.Context, string, bool) (*CompatibilityLevel, error) {
	return nil, errNotImplemented
}

// SetCredentials is not implemented
func (mck *MockSchemaRegistryClient) SetCredentials(string, string) {
	// Nothing because mockSchemaRegistryClient is actually very vulnerable
}

// SetBearerToken is not implemented
func (mck *MockSchemaRegistryClient) SetBearerToken(TokenProvider) {
	// Nothing because mockSchemaRegistryClient is actually very vulnerable
}

// SetTimeout is not implemented
func (mck *MockSchemaRegistryClient) SetTimeout(time.Duration) {
	// Nothing because there is no timeout for cache
}

// CachingEnabled is not implemented
func (mck *MockSchemaRegistryClient) CachingEnabled(bool) {
	// Nothing because caching is always enabled, duh
}

// ResetCache is not implemented
func (mck *MockSchemaRegistryClient) ResetCache() {
	// Nothing because there is no lock for cache
}

// CodecCreationEnabled is not implemented
func (mck *MockSchemaRegistryClient) CodecCreationEnabled(bool) {
	// Nothing because codecs do not matter in the inMem storage of schemas
}

// IsSchemaCompatible is not implemented
func (mck *MockSchemaRegistryClient) IsSchemaCompatible(context.Context, string, string, string, SchemaType, ...Reference) (bool, error) {
	return false, errNotImplemented
}

// LookupSchema is not implemented
func (mck *MockSchemaRegistryClient) LookupSchema(context.Context, string, string, SchemaType, ...Reference) (*Schema, error) {
	return nil, errNotImplemented
}

/*
These classes are written as helpers and therefore, are not exported.
generateVersion will register a new version of the schema passed, it will NOT do any checks
for the schema being already registered, or for the advancing of the schema ID, these are expected to be
handled beforehand by the environment.
allVersions returns an ordered int[] with all versions for a given subject. It does NOT
qualify for key/value subjects, it expects to have a `concrete subject` passed on to do the checks.
*/

// generateVersion the next version of the schema for the given subject, givenVersion can be set to -1 to generate one.
func (mck *MockSchemaRegistryClient) generateVersion(id int, subject string, schema string, schemaType SchemaType, givenVersion int) (*Schema, error) {
	schemaVersionMap := map[int]*Schema{}
	currentVersion := 1

	if givenVersion >= 0 {
		currentVersion = givenVersion
	}

	// if existing versions are found, make sure to load in the version map
	if existingMap := mck.schemaVersions[subject]; len(existingMap) > 0 {
		schemaVersionMap = existingMap

		// If no version was given, and existing versions are found, +1 the new number from the latest version
		if givenVersion <= 0 {
			versions := mck.allVersions(subject)
			currentVersion = versions[len(versions)-1] + 1
		}
	}

	// Add a codec, required otherwise Codec() panics and the mock registry is unusable
	codec, _ := goavro.NewCodec(schema)

	schemaToRegister := &Schema{
		id:         id,
		schema:     schema,
		version:    currentVersion,
		codec:      codec,
		schemaType: &schemaType,
	}

	schemaVersionMap[currentVersion] = schemaToRegister
	mck.schemaVersions[subject] = schemaVersionMap
	mck.schemaIDs[schemaToRegister.id] = schemaToRegister

	return schemaToRegister, nil
}

// allVersions returns all versions for a given subject, assumes it exists
func (mck *MockSchemaRegistryClient) allVersions(subject string) []int {
	var versions []int
	result, ok := mck.schemaVersions[subject]

	if ok {
		for version := range result {
			versions = append(versions, version)
		}
	}

	sort.Ints(versions)

	return versions
}
