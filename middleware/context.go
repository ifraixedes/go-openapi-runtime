// Copyright 2015 go-swagger maintainers
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package middleware

import (
	stdContext "context"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/go-openapi/analysis"
	"github.com/go-openapi/errors"
	"github.com/go-openapi/loads"
	"github.com/go-openapi/runtime"
	"github.com/go-openapi/runtime/middleware/untyped"
	"github.com/go-openapi/runtime/security"
	"github.com/go-openapi/spec"
	"github.com/go-openapi/strfmt"
)

// Debug when true turns on verbose logging
var Debug = os.Getenv("SWAGGER_DEBUG") != "" || os.Getenv("DEBUG") != ""

func debugLog(format string, args ...interface{}) {
	if Debug {
		log.Printf(format, args...)
	}
}

// A Builder can create middlewares
type Builder func(http.Handler) http.Handler

// PassthroughBuilder returns the handler, aka the builder identity function
func PassthroughBuilder(handler http.Handler) http.Handler { return handler }

// RequestBinder is an interface for types to implement
// when they want to be able to bind from a request
type RequestBinder interface {
	BindRequest(*http.Request, *MatchedRoute) error
}

// Responder is an interface for types to implement
// when they want to be considered for writing HTTP responses
type Responder interface {
	WriteResponse(http.ResponseWriter, runtime.Producer)
}

// ResponderFunc wraps a func as a Responder interface
type ResponderFunc func(http.ResponseWriter, runtime.Producer)

// WriteResponse writes to the response
func (fn ResponderFunc) WriteResponse(rw http.ResponseWriter, pr runtime.Producer) {
	fn(rw, pr)
}

// Context is a type safe wrapper around an untyped request context
// used throughout to store request context with the gorilla context module
type Context struct {
	spec     *loads.Document
	analyzer *analysis.Spec
	api      RoutableAPI
	router   Router
	formats  strfmt.Registry
}

type routableUntypedAPI struct {
	api             *untyped.API
	hlock           *sync.Mutex
	handlers        map[string]map[string]http.Handler
	defaultConsumes string
	defaultProduces string
}

func newRoutableUntypedAPI(spec *loads.Document, api *untyped.API, context *Context) *routableUntypedAPI {
	var handlers map[string]map[string]http.Handler
	if spec == nil || api == nil {
		return nil
	}
	analyzer := analysis.New(spec.Spec())
	for method, hls := range analyzer.Operations() {
		um := strings.ToUpper(method)
		for path, op := range hls {
			schemes := analyzer.SecurityDefinitionsFor(op)

			if oh, ok := api.OperationHandlerFor(method, path); ok {
				if handlers == nil {
					handlers = make(map[string]map[string]http.Handler)
				}
				if b, ok := handlers[um]; !ok || b == nil {
					handlers[um] = make(map[string]http.Handler)
				}

				var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// lookup route info in the context
					route, rCtx, _ := context.RouteInfo(r)
					if rCtx != nil {
						r = rCtx
					}

					// bind and validate the request using reflection
					var bound interface{}
					var validation error
					bound, r, validation = context.BindAndValidate(r, route)
					if validation != nil {
						context.Respond(w, r, route.Produces, route, validation)
						return
					}

					// actually handle the request
					result, err := oh.Handle(bound)
					if err != nil {
						// respond with failure
						context.Respond(w, r, route.Produces, route, err)
						return
					}

					// respond with success
					context.Respond(w, r, route.Produces, route, result)
				})

				if len(schemes) > 0 {
					handler = newSecureAPI(context, handler)
				}
				handlers[um][path] = handler
			}
		}
	}

	return &routableUntypedAPI{
		api:             api,
		hlock:           new(sync.Mutex),
		handlers:        handlers,
		defaultProduces: api.DefaultProduces,
		defaultConsumes: api.DefaultConsumes,
	}
}

func (r *routableUntypedAPI) HandlerFor(method, path string) (http.Handler, bool) {
	r.hlock.Lock()
	paths, ok := r.handlers[strings.ToUpper(method)]
	if !ok {
		r.hlock.Unlock()
		return nil, false
	}
	handler, ok := paths[path]
	r.hlock.Unlock()
	return handler, ok
}
func (r *routableUntypedAPI) ServeErrorFor(operationID string) func(http.ResponseWriter, *http.Request, error) {
	return r.api.ServeError
}
func (r *routableUntypedAPI) ConsumersFor(mediaTypes []string) map[string]runtime.Consumer {
	return r.api.ConsumersFor(mediaTypes)
}
func (r *routableUntypedAPI) ProducersFor(mediaTypes []string) map[string]runtime.Producer {
	return r.api.ProducersFor(mediaTypes)
}
func (r *routableUntypedAPI) AuthenticatorsFor(schemes map[string]spec.SecurityScheme) map[string]runtime.Authenticator {
	return r.api.AuthenticatorsFor(schemes)
}
func (r *routableUntypedAPI) Formats() strfmt.Registry {
	return r.api.Formats()
}

func (r *routableUntypedAPI) DefaultProduces() string {
	return r.defaultProduces
}

func (r *routableUntypedAPI) DefaultConsumes() string {
	return r.defaultConsumes
}

// NewRoutableContext creates a new context for a routable API
func NewRoutableContext(spec *loads.Document, routableAPI RoutableAPI, routes Router) *Context {
	var an *analysis.Spec
	if spec != nil {
		an = analysis.New(spec.Spec())
	}
	ctx := &Context{spec: spec, api: routableAPI, analyzer: an}
	return ctx
}

// NewContext creates a new context wrapper
func NewContext(spec *loads.Document, api *untyped.API, routes Router) *Context {
	var an *analysis.Spec
	if spec != nil {
		an = analysis.New(spec.Spec())
	}
	ctx := &Context{spec: spec, analyzer: an}
	ctx.api = newRoutableUntypedAPI(spec, api, ctx)
	return ctx
}

// Serve serves the specified spec with the specified api registrations as a http.Handler
func Serve(spec *loads.Document, api *untyped.API) http.Handler {
	return ServeWithBuilder(spec, api, PassthroughBuilder)
}

// ServeWithBuilder serves the specified spec with the specified api registrations as a http.Handler that is decorated
// by the Builder
func ServeWithBuilder(spec *loads.Document, api *untyped.API, builder Builder) http.Handler {
	context := NewContext(spec, api, nil)
	return context.APIHandler(builder)
}

type contextKey int8

const (
	_ contextKey = iota
	ctxContentType
	ctxResponseFormat
	ctxMatchedRoute
	ctxAllowedMethods
	ctxBoundParams
	ctxSecurityPrincipal
	ctxSecurityScopes

	ctxConsumer
)

type contentTypeValue struct {
	MediaType string
	Charset   string
}

// BasePath returns the base path for this API
func (c *Context) BasePath() string {
	return c.spec.BasePath()
}

// RequiredProduces returns the accepted content types for responses
func (c *Context) RequiredProduces() []string {
	return c.analyzer.RequiredProduces()
}

// BindValidRequest binds a params object to a request but only when the request is valid
// if the request is not valid an error will be returned
func (c *Context) BindValidRequest(request *http.Request, route *MatchedRoute, binder RequestBinder) error {
	var res []error

	requestContentType := "*/*"
	// check and validate content type, select consumer
	if runtime.HasBody(request) {
		ct, _, err := runtime.ContentType(request.Header)
		if err != nil {
			res = append(res, err)
		} else {
			if err := validateContentType(route.Consumes, ct); err != nil {
				res = append(res, err)
			}
			if len(res) == 0 {
				cons, ok := route.Consumers[ct]
				if !ok {
					res = append(res, errors.New(500, "no consumer registered for %s", ct))
				} else {
					route.Consumer = cons
					requestContentType = ct
				}
			}
		}
	}

	// check and validate the response format
	if len(res) == 0 && runtime.HasBody(request) {
		if str := NegotiateContentType(request, route.Produces, requestContentType); str == "" {
			res = append(res, errors.InvalidResponseFormat(request.Header.Get(runtime.HeaderAccept), route.Produces))
		}
	}

	// now bind the request with the provided binder
	// it's assumed the binder will also validate the request and return an error if the
	// request is invalid
	if binder != nil && len(res) == 0 {
		if err := binder.BindRequest(request, route); err != nil {
			return err
		}
	}

	if len(res) > 0 {
		return errors.CompositeValidationError(res...)
	}
	return nil
}

// ContentType gets the parsed value of a content type
// Returns the media type, its charset and a shallow copy of the request
// when its context doesn't contain the content type value, otherwise it returns
// the same request
// Returns the error that runtime.ContentType may retunrs.
func (c *Context) ContentType(request *http.Request) (string, string, *http.Request, error) {
	var rCtx = request.Context()

	if v, ok := rCtx.Value(ctxContentType).(*contentTypeValue); ok {
		return v.MediaType, v.Charset, request, nil
	}

	mt, cs, err := runtime.ContentType(request.Header)
	if err != nil {
		return "", "", nil, err
	}
	rCtx = stdContext.WithValue(rCtx, ctxContentType, &contentTypeValue{mt, cs})
	return mt, cs, request.WithContext(rCtx), nil
}

// LookupRoute looks a route up and returns true when it is found
func (c *Context) LookupRoute(request *http.Request) (*MatchedRoute, bool) {
	if route, ok := c.router.Lookup(request.Method, request.URL.EscapedPath()); ok {
		return route, ok
	}
	return nil, false
}

// RouteInfo tries to match a route for this request
// Returns the matched route, a shallow copy of the request if its context
// contains the matched router, otherwise the same request, and a bool to
// indicate if it the request matches one of the routes, if it doesn't
// then it returns false and nil for the other two return values
func (c *Context) RouteInfo(request *http.Request) (*MatchedRoute, *http.Request, bool) {
	var rCtx = request.Context()

	if v, ok := rCtx.Value(ctxMatchedRoute).(*MatchedRoute); ok {
		return v, request, ok
	}

	if route, ok := c.LookupRoute(request); ok {
		rCtx = stdContext.WithValue(rCtx, ctxMatchedRoute, route)
		return route, request.WithContext(rCtx), ok
	}

	return nil, nil, false
}

// ResponseFormat negotiates the response content type
// Returns the response format and a shallow copy of the request if its context
// doesn't contain the response format, otherwise the same request
func (c *Context) ResponseFormat(r *http.Request, offers []string) (string, *http.Request) {
	var rCtx = r.Context()

	if v, ok := rCtx.Value(ctxResponseFormat).(string); ok {
		return v, r
	}

	format := NegotiateContentType(r, offers, "")
	if format != "" {
		r = r.WithContext(stdContext.WithValue(rCtx, ctxResponseFormat, format))
	}
	return format, r
}

// AllowedMethods gets the allowed methods for the path of this request
func (c *Context) AllowedMethods(request *http.Request) []string {
	return c.router.OtherMethods(request.Method, request.URL.EscapedPath())
}

// Authorize authorizes the request
// Returns the principal object and a shallow copy of the request when its
// context doesn't contain the principal, otherwise the same request or an error
// (the last) if one of the authenticators returns one or an Unauthenticated error
func (c *Context) Authorize(request *http.Request, route *MatchedRoute) (interface{}, *http.Request, error) {
	if route == nil || len(route.Authenticators) == 0 {
		return nil, nil, nil
	}

	var rCtx = request.Context()
	if v := rCtx.Value(ctxSecurityPrincipal); v != nil {
		return v, request, nil
	}

	var lastError error
	for scheme, authenticator := range route.Authenticators {
		applies, usr, err := authenticator.Authenticate(&security.ScopedAuthRequest{
			Request:        request,
			RequiredScopes: route.Scopes[scheme],
		})
		if !applies || err != nil || usr == nil {
			if err != nil {
				lastError = err
			}
			continue
		}
		rCtx = stdContext.WithValue(rCtx, ctxSecurityPrincipal, usr)
		rCtx = stdContext.WithValue(rCtx, ctxSecurityScopes, route.Scopes[scheme])
		return usr, request.WithContext(rCtx), nil
	}

	if lastError != nil {
		return nil, nil, lastError
	}

	return nil, nil, errors.Unauthenticated("invalid credentials")
}

// BindAndValidate binds and validates the request
// Returns the validation map and a shallow copy of the request when its context
// doesn't contain the validation, otherwise it returns the same request or an
// CompositeValidationError error
func (c *Context) BindAndValidate(request *http.Request, matched *MatchedRoute) (interface{}, *http.Request, error) {
	var rCtx = request.Context()

	if v, ok := rCtx.Value(ctxBoundParams).(*validation); ok {
		debugLog("got cached validation (valid: %t)", len(v.result) == 0)
		if len(v.result) > 0 {
			return v.bound, request, errors.CompositeValidationError(v.result...)
		}
		return v.bound, request, nil
	}
	result := validateRequest(c, request, matched)
	rCtx = stdContext.WithValue(rCtx, ctxBoundParams, result)
	request = request.WithContext(rCtx)
	if len(result.result) > 0 {
		return result.bound, request, errors.CompositeValidationError(result.result...)
	}
	debugLog("no validation errors found")
	return result.bound, request, nil
}

// NotFound the default not found responder for when no route has been matched yet
func (c *Context) NotFound(rw http.ResponseWriter, r *http.Request) {
	c.Respond(rw, r, []string{c.api.DefaultProduces()}, nil, errors.NotFound("not found"))
}

// Respond renders the response after doing some content negotiation
func (c *Context) Respond(rw http.ResponseWriter, r *http.Request, produces []string, route *MatchedRoute, data interface{}) {
	offers := []string{}
	for _, mt := range produces {
		if mt != c.api.DefaultProduces() {
			offers = append(offers, mt)
		}
	}
	// the default producer is last so more specific producers take precedence
	offers = append(offers, c.api.DefaultProduces())

	var format string
	format, r = c.ResponseFormat(r, offers)
	rw.Header().Set(runtime.HeaderContentType, format)

	if resp, ok := data.(Responder); ok {
		producers := route.Producers
		prod, ok := producers[format]
		if !ok {
			prods := c.api.ProducersFor([]string{c.api.DefaultProduces()})
			pr, ok := prods[c.api.DefaultProduces()]
			if !ok {
				panic(errors.New(http.StatusInternalServerError, "can't find a producer for "+format))
			}
			prod = pr
		}
		resp.WriteResponse(rw, prod)
		return
	}

	if err, ok := data.(error); ok {
		if format == "" {
			rw.Header().Set(runtime.HeaderContentType, runtime.JSONMime)
		}
		if route == nil || route.Operation == nil {
			c.api.ServeErrorFor("")(rw, r, err)
			return
		}
		c.api.ServeErrorFor(route.Operation.ID)(rw, r, err)
		return
	}

	if route == nil || route.Operation == nil {
		rw.WriteHeader(200)
		if r.Method == "HEAD" {
			return
		}
		producers := c.api.ProducersFor(offers)
		prod, ok := producers[format]
		if !ok {
			panic(errors.New(http.StatusInternalServerError, "can't find a producer for "+format))
		}
		if err := prod.Produce(rw, data); err != nil {
			panic(err) // let the recovery middleware deal with this
		}
		return
	}

	if _, code, ok := route.Operation.SuccessResponse(); ok {
		rw.WriteHeader(code)
		if code == 204 || r.Method == "HEAD" {
			return
		}

		producers := route.Producers
		prod, ok := producers[format]
		if !ok {
			if !ok {
				prods := c.api.ProducersFor([]string{c.api.DefaultProduces()})
				pr, ok := prods[c.api.DefaultProduces()]
				if !ok {
					panic(errors.New(http.StatusInternalServerError, "can't find a producer for "+format))
				}
				prod = pr
			}
		}
		if err := prod.Produce(rw, data); err != nil {
			panic(err) // let the recovery middleware deal with this
		}
		return
	}

	c.api.ServeErrorFor(route.Operation.ID)(rw, r, errors.New(http.StatusInternalServerError, "can't produce response"))
}

// APIHandler returns a handler to serve the API, this includes a swagger spec, router and the contract defined in the swagger spec
func (c *Context) APIHandler(builder Builder) http.Handler {
	b := builder
	if b == nil {
		b = PassthroughBuilder
	}

	var title string
	sp := c.spec.Spec()
	if sp != nil && sp.Info != nil && sp.Info.Title != "" {
		title = sp.Info.Title
	}

	redocOpts := RedocOpts{
		BasePath: c.BasePath(),
		Title:    title,
	}

	return Spec("", c.spec.Raw(), Redoc(redocOpts, c.RoutesHandler(builder)))
}

// RoutesHandler returns a handler to serve the API, just the routes and the contract defined in the swagger spec
func (c *Context) RoutesHandler(builder Builder) http.Handler {
	b := builder
	if b == nil {
		b = PassthroughBuilder
	}
	return NewRouter(c, b(NewOperationExecutor(c)))
}
