package http

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/corpix/uarand"
	"github.com/pkg/errors"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/nuclei/v2/pkg/protocols/common/contextargs"
	"github.com/projectdiscovery/nuclei/v2/pkg/protocols/common/expressions"
	"github.com/projectdiscovery/nuclei/v2/pkg/protocols/common/generators"
	"github.com/projectdiscovery/nuclei/v2/pkg/protocols/common/replacer"
	"github.com/projectdiscovery/nuclei/v2/pkg/protocols/common/utils/vardump"
	"github.com/projectdiscovery/nuclei/v2/pkg/protocols/http/race"
	"github.com/projectdiscovery/nuclei/v2/pkg/protocols/http/raw"
	"github.com/projectdiscovery/nuclei/v2/pkg/protocols/http/utils"
	"github.com/projectdiscovery/nuclei/v2/pkg/types"
	"github.com/projectdiscovery/rawhttp"
	"github.com/projectdiscovery/retryablehttp-go"
	readerutil "github.com/projectdiscovery/utils/reader"
	stringsutil "github.com/projectdiscovery/utils/strings"
	urlutil "github.com/projectdiscovery/utils/url"
)

var (
	urlWithPortRegex = regexp.MustCompile(`{{BaseURL}}:(\d+)`)
)

const evaluateHelperExpressionErrorMessage = "could not evaluate helper expressions"

// generatedRequest is a single generated request wrapped for a template request
type generatedRequest struct {
	original             *Request
	rawRequest           *raw.Request
	meta                 map[string]interface{}
	pipelinedClient      *rawhttp.PipelineClient
	request              *retryablehttp.Request
	dynamicValues        map[string]interface{}
	interactshURLs       []string
	customCancelFunction context.CancelFunc
}

func (g *generatedRequest) URL() string {
	if g.request != nil {
		return g.request.URL.String()
	}
	if g.rawRequest != nil {
		return g.rawRequest.FullURL
	}
	return ""
}

// Make creates a http request for the provided input.
// It returns io.EOF as error when all the requests have been exhausted.
func (r *requestGenerator) Make(ctx context.Context, input *contextargs.Context, data string, payloads, dynamicValues map[string]interface{}) (*generatedRequest, error) {
	if r.request.SelfContained {
		return r.makeSelfContainedRequest(ctx, data, payloads, dynamicValues)
	}
	if r.options.Interactsh != nil {
		data, r.interactshURLs = r.options.Interactsh.ReplaceMarkers(data, []string{})
		for payloadName, payloadValue := range payloads {
			payloads[payloadName], r.interactshURLs = r.options.Interactsh.ReplaceMarkers(types.ToString(payloadValue), r.interactshURLs)
		}
	} else {
		for payloadName, payloadValue := range payloads {
			payloads[payloadName] = types.ToString(payloadValue)
		}
	}
	parsed, err := urlutil.Parse(input.MetaInput.Input)
	if err != nil {
		return nil, err
	}
	isRawRequest := len(r.request.Raw) > 0

	// if path contains port ex: {{BaseURL}}:8080 use port
	parsed, data = UsePortFromPayload(parsed, data)

	// If not raw request process input values
	if !isRawRequest {
		data, parsed = addParamsToBaseURL(data, parsed)
	}

	// If the request is not a raw request, and the URL input path is suffixed with
	// a trailing slash, and our Input URL is also suffixed with a trailing slash,
	// mark trailingSlash bool as true which will be later used during variable generation
	// to generate correct path removed slash which would otherwise generate // invalid sequence.
	// TODO: Figure out a cleaner way to do this sanitization.
	trailingSlash := false
	if !isRawRequest && strings.HasSuffix(parsed.Path, "/") && strings.Contains(data, "{{BaseURL}}/") {
		trailingSlash = true
	}

	values := generators.MergeMaps(
		generators.MergeMaps(dynamicValues, utils.GenerateVariablesWithURL(parsed, trailingSlash, contextargs.GenerateVariables(input))),
		generators.BuildPayloadFromOptions(r.request.options.Options),
	)
	if vardump.EnableVarDump {
		gologger.Debug().Msgf("Protocol request variables: \n%s\n", vardump.DumpVariables(values))
	}

	// If data contains \n it's a raw request, process it like raw. Else
	// continue with the template based request flow.
	if isRawRequest {
		return r.makeHTTPRequestFromRaw(ctx, parsed.String(), data, values, payloads)
	}
	return r.makeHTTPRequestFromModel(ctx, data, values, payloads)
}

func (r *requestGenerator) makeSelfContainedRequest(ctx context.Context, data string, payloads, dynamicValues map[string]interface{}) (*generatedRequest, error) {
	isRawRequest := r.request.isRaw()

	// If the request is a raw request, get the URL from the request
	// header and use it to make the request.
	if isRawRequest {
		// Get the hostname from the URL section to build the request.
		reader := bufio.NewReader(strings.NewReader(data))
	read_line:
		s, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("could not read request: %w", err)
		}
		// ignore all annotations
		if stringsutil.HasPrefixAny(s, "@") {
			goto read_line
		}

		parts := strings.Split(s, " ")
		if len(parts) < 3 {
			return nil, fmt.Errorf("malformed request supplied")
		}

		values := generators.MergeMaps(
			payloads,
			generators.BuildPayloadFromOptions(r.request.options.Options),
		)

		// in case cases (eg requests signing, some variables uses default values if missing)
		if defaultList := GetVariablesDefault(r.request.Signature.Value); defaultList != nil {
			values = generators.MergeMaps(defaultList, values)
		}

		parts[1] = replacer.Replace(parts[1], values)
		if len(dynamicValues) > 0 {
			parts[1] = replacer.Replace(parts[1], dynamicValues)
		}

		// the url might contain placeholders with ignore list
		if ignoreList := GetVariablesNamesSkipList(r.request.Signature.Value); ignoreList != nil {
			if err := expressions.ContainsVariablesWithIgnoreList(ignoreList, parts[1]); err != nil {
				return nil, err
			}
		} else if err := expressions.ContainsUnresolvedVariables(parts[1]); err != nil { // the url might contain placeholders
			return nil, err
		}

		parsed, err := urlutil.ParseURL(parts[1], true)
		if err != nil {
			return nil, fmt.Errorf("could not parse request URL: %w", err)
		}
		values = generators.MergeMaps(
			generators.MergeMaps(dynamicValues, utils.GenerateVariablesWithURL(parsed, false, nil)),
			values,
		)

		return r.makeHTTPRequestFromRaw(ctx, parsed.String(), data, values, payloads)
	}
	values := generators.MergeMaps(
		dynamicValues,
		generators.BuildPayloadFromOptions(r.request.options.Options),
	)
	return r.makeHTTPRequestFromModel(ctx, data, values, payloads)
}

// Total returns the total number of requests for the generator
func (r *requestGenerator) Total() int {
	if r.payloadIterator != nil {
		return len(r.request.Raw) * r.payloadIterator.Remaining()
	}
	return len(r.request.Path)
}

// MakeHTTPRequestFromModel creates a *http.Request from a request template
func (r *requestGenerator) makeHTTPRequestFromModel(ctx context.Context, data string, values, generatorValues map[string]interface{}) (*generatedRequest, error) {
	if r.options.Interactsh != nil {
		data, r.interactshURLs = r.options.Interactsh.ReplaceMarkers(data, r.interactshURLs)
	}

	// Combine the template payloads along with base
	// request values.
	finalValues := generators.MergeMaps(generatorValues, values)

	// Evaluate the expressions for the request if any.
	var err error
	data, err = expressions.Evaluate(data, finalValues)
	if err != nil {
		return nil, errors.Wrap(err, evaluateHelperExpressionErrorMessage)
	}

	method, err := expressions.Evaluate(r.request.Method.String(), finalValues)
	if err != nil {
		return nil, errors.Wrap(err, evaluateHelperExpressionErrorMessage)
	}

	// Build a request on the specified URL
	req, err := retryablehttp.NewRequestWithContext(ctx, method, data, nil)
	if err != nil {
		return nil, err
	}

	request, err := r.fillRequest(req, finalValues)
	if err != nil {
		return nil, err
	}
	return &generatedRequest{request: request, meta: generatorValues, original: r.request, dynamicValues: finalValues, interactshURLs: r.interactshURLs}, nil
}

// makeHTTPRequestFromRaw creates a *http.Request from a raw request
func (r *requestGenerator) makeHTTPRequestFromRaw(ctx context.Context, baseURL, data string, values, payloads map[string]interface{}) (*generatedRequest, error) {
	if r.options.Interactsh != nil {
		data, r.interactshURLs = r.options.Interactsh.ReplaceMarkers(data, r.interactshURLs)
	}
	return r.handleRawWithPayloads(ctx, data, baseURL, values, payloads)
}

// handleRawWithPayloads handles raw requests along with payloads
func (r *requestGenerator) handleRawWithPayloads(ctx context.Context, rawRequest, baseURL string, values, generatorValues map[string]interface{}) (*generatedRequest, error) {
	// Combine the template payloads along with base
	// request values.
	finalValues := generators.MergeMaps(generatorValues, values)

	// Evaluate the expressions for raw request if any.
	var err error
	rawRequest, err = expressions.Evaluate(rawRequest, finalValues)
	if err != nil {
		return nil, errors.Wrap(err, evaluateHelperExpressionErrorMessage)
	}
	rawRequestData, err := raw.Parse(rawRequest, baseURL, r.request.Unsafe)
	if err != nil {
		return nil, err
	}

	// Unsafe option uses rawhttp library
	if r.request.Unsafe {
		if len(r.options.Options.CustomHeaders) > 0 {
			_ = rawRequestData.TryFillCustomHeaders(r.options.Options.CustomHeaders)
		}
		unsafeReq := &generatedRequest{rawRequest: rawRequestData, meta: generatorValues, original: r.request, interactshURLs: r.interactshURLs}
		return unsafeReq, nil
	}

	var body io.ReadCloser
	body = io.NopCloser(strings.NewReader(rawRequestData.Data))
	if r.request.Race {
		// More or less this ensures that all requests hit the endpoint at the same approximated time
		// Todo: sync internally upon writing latest request byte
		body = race.NewOpenGateWithTimeout(body, time.Duration(2)*time.Second)
	}

	req, err := retryablehttp.NewRequestWithContext(ctx, rawRequestData.Method, rawRequestData.FullURL, body)
	if err != nil {
		return nil, err
	}
	for key, value := range rawRequestData.Headers {
		if key == "" {
			continue
		}
		req.Header[key] = []string{value}
		if key == "Host" {
			req.Host = value
		}
	}
	request, err := r.fillRequest(req, finalValues)
	if err != nil {
		return nil, err
	}

	generatedRequest := &generatedRequest{
		request:        request,
		meta:           generatorValues,
		original:       r.request,
		dynamicValues:  finalValues,
		interactshURLs: r.interactshURLs,
	}

	if reqWithAnnotations, cancelFunc, hasAnnotations := r.request.parseAnnotations(rawRequest, req); hasAnnotations {
		generatedRequest.request = reqWithAnnotations
		generatedRequest.customCancelFunction = cancelFunc
	}

	return generatedRequest, nil
}

// fillRequest fills various headers in the request with values
func (r *requestGenerator) fillRequest(req *retryablehttp.Request, values map[string]interface{}) (*retryablehttp.Request, error) {
	// Set the header values requested
	for header, value := range r.request.Headers {
		if r.options.Interactsh != nil {
			value, r.interactshURLs = r.options.Interactsh.ReplaceMarkers(value, r.interactshURLs)
		}
		value, err := expressions.Evaluate(value, values)
		if err != nil {
			return nil, errors.Wrap(err, evaluateHelperExpressionErrorMessage)
		}
		req.Header[header] = []string{value}
		if header == "Host" {
			req.Host = value
		}
	}

	// In case of multiple threads the underlying connection should remain open to allow reuse
	if r.request.Threads <= 0 && req.Header.Get("Connection") == "" {
		req.Close = true
	}

	// Check if the user requested a request body
	if r.request.Body != "" {
		body := r.request.Body
		if r.options.Interactsh != nil {
			body, r.interactshURLs = r.options.Interactsh.ReplaceMarkers(r.request.Body, r.interactshURLs)
		}
		body, err := expressions.Evaluate(body, values)
		if err != nil {
			return nil, errors.Wrap(err, evaluateHelperExpressionErrorMessage)
		}
		bodyreader, err := readerutil.NewReusableReadCloser([]byte(body))
		if err != nil {
			return nil, errors.Wrap(err, "failed to create reusable reader for request body")
		}
		req.Body = bodyreader
	}
	if !r.request.Unsafe {
		setHeader(req, "User-Agent", uarand.GetRandom())
	}

	// Only set these headers on non-raw requests
	if len(r.request.Raw) == 0 && !r.request.Unsafe {
		setHeader(req, "Accept", "*/*")
		setHeader(req, "Accept-Language", "en")
	}

	if !LeaveDefaultPorts {
		switch {
		case req.URL.Scheme == "http" && strings.HasSuffix(req.Host, ":80"):
			req.Host = strings.TrimSuffix(req.Host, ":80")
		case req.URL.Scheme == "https" && strings.HasSuffix(req.Host, ":443"):
			req.Host = strings.TrimSuffix(req.Host, ":443")
		}
	}

	if r.request.DigestAuthUsername != "" {
		req.Auth = &retryablehttp.Auth{
			Type:     retryablehttp.DigestAuth,
			Username: r.request.DigestAuthUsername,
			Password: r.request.DigestAuthPassword,
		}
	}

	return req, nil
}

// setHeader sets some headers only if the header wasn't supplied by the user
func setHeader(req *retryablehttp.Request, name, value string) {
	if _, ok := req.Header[name]; !ok {
		req.Header.Set(name, value)
	}
	if name == "Host" {
		req.Host = value
	}
}

// UsePortFromPayload overrides input port if specified in payload(ex: {{BaseURL}}:8080)
func UsePortFromPayload(parsed *urlutil.URL, data string) (*urlutil.URL, string) {
	matches := urlWithPortRegex.FindAllStringSubmatch(data, -1)
	if len(matches) > 0 {
		port := matches[0][1]
		parsed.UpdatePort(port)
		// remove it from dsl
		data = strings.Replace(data, ":"+port, "", 1)
	}
	return parsed, data
}

// If input/target contains any parameters add them to payload preserving the order
func addParamsToBaseURL(data string, parsed *urlutil.URL) (string, *urlutil.URL) {
	// preprocess
	payloadPath := strings.TrimPrefix(data, "{{BaseURL}}")
	if strings.HasSuffix(parsed.Path, "/") && strings.HasPrefix(payloadPath, "/") {
		// keeping payload intact trim extra slash from input
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	}

	if payloadPath == "" {
		return data, parsed
	}
	if strings.HasPrefix(payloadPath, "?") {
		// does not contain path only params
		payloadPath = strings.TrimPrefix(payloadPath, "?")
		payloadParams := make(urlutil.Params)
		payloadParams.Decode(payloadPath)
		payloadParams.Merge(parsed.Params)
		return "{{BaseURL}}?" + payloadParams.Encode(), parsed
	}

	// If payload has path parse it add automerge parameters with proper preference
	payloadURL, err := urlutil.ParseURL(payloadPath, true)
	if err != nil {
		gologger.Debug().Msgf("failed to parse payload %v and %v.skipping param merge", data, parsed.String())
		return data, parsed
	}
	payloadURL.Params.Merge(parsed.Params)
	payloadPath = "{{BaseURL}}" + payloadURL.GetRelativePath()
	return payloadPath, parsed
}
