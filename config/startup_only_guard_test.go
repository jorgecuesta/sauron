package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// configFieldClass classifies every leaf field of MultiChainConfig by when a
// change to it takes effect: "startup" fields keep their running value on a
// reload (keepStartupOnly), "live" fields apply immediately. A field nothing
// reads yet is "live": changing it is accepted and simply has no effect.
//
// Adding a config field fails TestConfigFieldsAreClassified until it is listed
// here, and TestKeepStartupOnlyMatchesClassification fails if keepStartupOnly
// disagrees with the entry. Before classifying a field, trace every constructor
// that copies it (see the keepStartupOnly doc comment).
var configFieldClass = map[string]string{
	"listen":                               "startup",
	"auth":                                 "startup",
	"timeouts.health_check":                "live",
	"timeouts.proxy":                       "startup",
	"timeouts.websocket_idle":              "live", // read per WebSocket connection in handleWebSocket
	"grpc_keepalive.time":                  "startup",
	"grpc_keepalive.timeout":               "startup",
	"grpc_keepalive.permit_without_stream": "startup",
	"redis.enabled":                        "startup",
	"redis.uri":                            "startup",
	"rate_limit.enabled":                   "startup",
	"rate_limit.requests_per_second":       "startup",
	"rate_limit.burst":                     "startup",
	"rate_limit.trust_proxy":               "startup",
	"external_failover_threshold":          "live",

	"networks[].name":                               "startup",
	"networks[].type":                               "startup",
	"networks[].mode":                               "startup",
	"networks[].height_check.protocol":              "startup",
	"networks[].height_check.interval":              "live", // unused: the height cron is fixed at 30s
	"networks[].height_check.method":                "startup",
	"networks[].height_check.url_path":              "startup",
	"networks[].height_check.headers":               "startup",
	"networks[].height_check.body":                  "startup",
	"networks[].height_check.response_path":         "startup",
	"networks[].height_check.response_format":       "startup",
	"networks[].endpoints[].protocol":               "startup",
	"networks[].endpoints[].listen":                 "startup",
	"networks[].endpoints[].advertise":              "live",
	"networks[].endpoints[].grpc_max_recv_msg_size": "startup",
	"networks[].endpoints[].grpc_max_send_msg_size": "startup",
	"networks[].archival.min_height":                "live",
	"networks[].archival.check_interval":            "live", // unused: the archival cron is fixed at 30s
	"networks[].sync.max_drift":                     "startup",
	"networks[].sync.check_interval":                "startup",
	"networks[].sync.oracles[].url":                 "startup",
	"networks[].sync.oracles[].method":              "startup",
	"networks[].sync.oracles[].url_path":            "startup",
	"networks[].sync.oracles[].headers":             "startup",
	"networks[].sync.oracles[].body":                "startup",
	"networks[].sync.oracles[].response_path":       "startup",
	"networks[].sync.oracles[].response_format":     "startup",
	"networks[].circuit_breaker.http_codes":         "live",
	"networks[].circuit_breaker.grpc_codes":         "live", // read per gRPC call by GRPCProxy.recordBreakerOutcome
	"networks[].circuit_breaker.threshold":          "live",
	"networks[].circuit_breaker.retry_attempts":     "live",
	"networks[].circuit_breaker.retry_backoff":      "live",

	"internals[].name":          "live",
	"internals[].network":       "live",
	"internals[].endpoints":     "live",
	"internals[].grpc_insecure": "startup", // while the gRPC URL is unchanged

	"externals[].name":  "live",
	"externals[].token": "live",
	"externals[].rings": "live",

	"users[].name":                 "live",
	"users[].token":                "live",
	"users[].permissions.All":      "live",
	"users[].permissions.Networks": "live",
}

// configLeaf is one leaf field: its path and how to reach it from the root.
type configLeaf struct {
	path string
	get  func(root reflect.Value) reflect.Value
}

// configLeaves walks MultiChainConfig and returns every leaf field, going
// through pointers and into the first element of struct slices.
func configLeaves() []configLeaf {
	var leaves []configLeaf
	var walk func(t reflect.Type, prefix string, nav func(reflect.Value) reflect.Value)
	walk = func(t reflect.Type, prefix string, nav func(reflect.Value) reflect.Value) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("mapstructure"), ",")[0]
			if name == "" {
				name = f.Name
			}
			idx := i
			path := prefix + name
			fieldNav := func(root reflect.Value) reflect.Value { return nav(root).Field(idx) }

			ft := f.Type
			switch {
			case ft.Kind() == reflect.Struct:
				walk(ft, path+".", fieldNav)
			case ft.Kind() == reflect.Pointer && ft.Elem().Kind() == reflect.Struct:
				walk(ft.Elem(), path+".", func(root reflect.Value) reflect.Value { return fieldNav(root).Elem() })
			case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
				walk(ft.Elem(), path+"[].", func(root reflect.Value) reflect.Value { return fieldNav(root).Index(0) })
			default:
				leaves = append(leaves, configLeaf{path: path, get: fieldNav})
			}
		}
	}
	walk(reflect.TypeOf(MultiChainConfig{}), "", func(root reflect.Value) reflect.Value { return root })
	return leaves
}

// fillConfig sets every field reachable from v to a non-zero value: pointers
// allocated, slices and maps given one element.
func fillConfig(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillConfig(v.Field(i))
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillConfig(v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillConfig(v.Index(0))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		val := reflect.New(v.Type().Elem()).Elem()
		fillConfig(val)
		m.SetMapIndex(reflect.ValueOf("k").Convert(v.Type().Key()), val)
		v.Set(m)
	case reflect.String:
		v.SetString("s")
	case reflect.Int, reflect.Int64:
		if v.Type() == reflect.TypeOf(time.Duration(0)) {
			v.SetInt(int64(time.Second))
			return
		}
		v.SetInt(1)
	case reflect.Bool:
		v.SetBool(false)
	}
}

// mutateLeaf changes a leaf to a different value of the same type.
func mutateLeaf(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "-changed")
	case reflect.Int, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillConfig(elem)
		v.Set(reflect.Append(v, elem))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		for _, k := range v.MapKeys() {
			m.SetMapIndex(k, v.MapIndex(k))
		}
		val := reflect.New(v.Type().Elem()).Elem()
		fillConfig(val)
		m.SetMapIndex(reflect.ValueOf("k2").Convert(v.Type().Key()), val)
		v.Set(m)
	default:
		t.Fatalf("mutateLeaf: unsupported kind %s", v.Kind())
	}
}

// filledConfig returns a config with every field set, built fresh each call.
func filledConfig() *MultiChainConfig {
	var c MultiChainConfig
	fillConfig(reflect.ValueOf(&c).Elem())
	return &c
}

// TestConfigFieldsAreClassified: every leaf field of MultiChainConfig must be
// classified, and every classification must name a real field.
func TestConfigFieldsAreClassified(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for _, leaf := range configLeaves() {
		seen[leaf.path] = true
		class, ok := configFieldClass[leaf.path]
		if !ok {
			t.Errorf("config field %q is not classified: add it to configFieldClass as \"startup\" or \"live\"", leaf.path)
			continue
		}
		if class != "startup" && class != "live" {
			t.Errorf("config field %q has class %q, want \"startup\" or \"live\"", leaf.path, class)
		}
	}
	var stale []string
	for path := range configFieldClass {
		if !seen[path] {
			stale = append(stale, path)
		}
	}
	sort.Strings(stale)
	for _, path := range stale {
		t.Errorf("configFieldClass lists %q, which is not a config field", path)
	}
}

// TestKeepStartupOnlyMatchesClassification changes one leaf at a time and
// checks keepStartupOnly against configFieldClass: a startup field must be
// reported and keep its running value, a live field must be applied silently.
func TestKeepStartupOnlyMatchesClassification(t *testing.T) {
	t.Parallel()

	for _, leaf := range configLeaves() {
		class, ok := configFieldClass[leaf.path]
		if !ok {
			continue // reported by TestConfigFieldsAreClassified
		}
		t.Run(leaf.path, func(t *testing.T) {
			old, changed := filledConfig(), filledConfig()
			mutateLeaf(t, leaf.get(reflect.ValueOf(changed).Elem()))

			eff, kept := keepStartupOnly(old, changed)
			got := leaf.get(reflect.ValueOf(eff).Elem()).Interface()
			oldVal := leaf.get(reflect.ValueOf(old).Elem()).Interface()
			newVal := leaf.get(reflect.ValueOf(changed).Elem()).Interface()

			switch class {
			case "startup":
				if len(kept) == 0 {
					t.Errorf("change not reported as needing a restart")
				}
				if !reflect.DeepEqual(got, oldVal) {
					t.Errorf("effective value = %v, want the running value %v", got, oldVal)
				}
			case "live":
				if len(kept) != 0 {
					t.Errorf("live change reported as needing a restart: %v", kept)
				}
				if !reflect.DeepEqual(got, newVal) {
					t.Errorf("effective value = %v, want the new value %v", got, newVal)
				}
			}
		})
	}
}
