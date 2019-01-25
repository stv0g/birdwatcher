package bird

import (
	"bytes"
	"io"
	"log"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"os/exec"
)

var ClientConf BirdConfig
var StatusConf StatusConfig
var IPVersion = "4"
var RateLimitConf struct {
	sync.RWMutex
	Conf RateLimitConfig
}

var CacheMap = struct {
	sync.RWMutex
	m map[string]Parsed
}{m: make(map[string]Parsed)}

var CacheRedis *RedisCache

var NilParse Parsed = (Parsed)(nil)
var BirdError Parsed = Parsed{"error": "bird unreachable"}

var FilteredCommunities = []string{
	"34307:60001",
	"34307:60002",
	"34307:60003",
	"34307:60004",
}

func isSpecial(ret Parsed) bool {
	return reflect.DeepEqual(ret, NilParse) || reflect.DeepEqual(ret, BirdError)
}

func isRouteFiltered(rdata interface{}) bool {
	// Get communities from parsed result
	route, ok := rdata.(map[string]interface{})
	if !ok {
		return false
	}
	bgpInfo, ok := route["bgp"].(map[string]interface{})
	if !ok {
		return false
	}

	communities := bgpInfo["communities"].([]interface{})
	for _, comdata := range communities {
		cdata, ok := comdata.([]interface{})
		if !ok {
			return false
		}
		if len(cdata) < 2 {
			return false
		}
		comm := strconv.Itoa(int(cdata[0].(float64))) + ":" +
			strconv.Itoa(int(cdata[1].(float64)))

		for _, filter := range FilteredCommunities {
			if comm == filter {
				return true
			}
		}
	}

	return false
}

func fromCacheMemory(key string) (Parsed, bool) {
	CacheMap.RLock()
	val, ok := CacheMap.m[key]
	CacheMap.RUnlock()
	if !ok {
		return NilParse, false
	}

	ttl, correct := val["ttl"].(time.Time)
	if !correct || ttl.Before(time.Now()) {
		return NilParse, false
	}

	return val, ok
}

func fromCacheRedis(key string) (Parsed, bool) {
	key = "B" + IPVersion + "_" + key
	val, err := CacheRedis.Get(key)
	if err != nil {
		return NilParse, false
	}

	ttl, correct := val["ttl"].(time.Time)
	if !correct || ttl.Before(time.Now()) {
		return NilParse, false
	}

	return val, true
}

func fromCache(key string) (Parsed, bool) {
	if CacheRedis == nil {
		return fromCacheMemory(key)
	}

	return fromCacheRedis(key)
}

func toCacheMemory(key string, val Parsed) {
	val["ttl"] = time.Now().Add(5 * time.Minute)
	CacheMap.Lock()
	CacheMap.m[key] = val
	CacheMap.Unlock()
}

func toCacheRedis(key string, val Parsed) {
	key = "B" + IPVersion + "_" + key
	val["ttl"] = time.Now().Add(5 * time.Minute)
	err := CacheRedis.Set(key, val)
	if err != nil {
		log.Println("Could not set cache for key:", key, "Error:", err)
	}
}

func toCache(key string, val Parsed) {
	if CacheRedis == nil {
		toCacheMemory(key, val)
	} else {
		toCacheRedis(key, val)
	}
}

func Run(args string) (io.Reader, error) {
	args = "show " + args
	argsList := strings.Split(args, " ")

	out, err := exec.Command(ClientConf.BirdCmd, argsList...).Output()
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(out), nil
}

func InstallRateLimitReset() {
	go func() {
		c := time.Tick(time.Second)

		for _ = range c {
			RateLimitConf.Lock()
			RateLimitConf.Conf.Reqs = RateLimitConf.Conf.Max
			RateLimitConf.Unlock()
		}
	}()
}

func checkRateLimit() bool {
	RateLimitConf.RLock()
	check := !RateLimitConf.Conf.Enabled
	RateLimitConf.RUnlock()
	if check {
		return true
	}

	RateLimitConf.RLock()
	check = RateLimitConf.Conf.Reqs < 1
	RateLimitConf.RUnlock()
	if check {
		return false
	}

	RateLimitConf.Lock()
	RateLimitConf.Conf.Reqs -= 1
	RateLimitConf.Unlock()

	return true
}

func RunAndParse(cmd string, parser func(io.Reader) Parsed) (Parsed, bool) {
	if val, ok := fromCache(cmd); ok {
		return val, true
	}

	if !checkRateLimit() {
		return NilParse, false
	}

	out, err := Run(cmd)
	if err != nil {
		// ignore errors for now
		return BirdError, false
	}

	parsed := parser(out)
	toCache(cmd, parsed)
	return parsed, false
}

func Status() (Parsed, bool) {
	birdStatus, ok := RunAndParse("status", parseStatus)
	if isSpecial(birdStatus) {
		return birdStatus, ok
	}
	status := birdStatus["status"].(Parsed)

	// Last Reconfig Timestamp source:
	var lastReconfig string
	switch StatusConf.ReconfigTimestampSource {
	case "bird":
		lastReconfig = status["last_reconfig"].(string)
		break
	case "config_modified":
		lastReconfig = lastReconfigTimestampFromFileStat(
			ClientConf.ConfigFilename,
		)
	case "config_regex":
		lastReconfig = lastReconfigTimestampFromFileContent(
			ClientConf.ConfigFilename,
			StatusConf.ReconfigTimestampMatch,
		)
	}

	status["last_reconfig"] = lastReconfig

	// Filter fields
	for _, field := range StatusConf.FilterFields {
		status[field] = nil
	}

	birdStatus["status"] = status

	return birdStatus, ok
}

func Protocols() (Parsed, bool) {
	return RunAndParse("protocols all", parseProtocols)
}

func ProtocolsBgp() (Parsed, bool) {
	p, from_cache := Protocols()
	if isSpecial(p) {
		return p, from_cache
	}
	protocols := p["protocols"].([]string)

	bgpProto := Parsed{}

	for _, v := range protocols {
		if strings.Contains(v, " BGP ") {
			key := strings.Split(v, " ")[0]
			bgpProto[key] = parseBgp(v)
		}
	}

	return Parsed{"protocols": bgpProto, "ttl": p["ttl"]}, from_cache
}

func Symbols() (Parsed, bool) {
	return RunAndParse("symbols", parseSymbols)
}

func RoutesPrefixed(prefix string) (Parsed, bool) {
	cmd := routeQueryForChannel("route all")
	return RunAndParse(cmd, parseRoutes)
}

func RoutesProtoAll(protocol string) (Parsed, bool) {
	cmd := routeQueryForChannel("route all protocol " + protocol)
	return RunAndParse(cmd, parseRoutes)
}

func RoutesProto(protocol string) (Parsed, bool) {
	// Get all routes
	data, fromCache := RoutesProtoAll(protocol)

	routes, ok := data["routes"].([]interface{})
	if !ok {
		return NilParse, false
	}

	// Remove all routes filtered
	cleanRoutes := make([]interface{}, 0, len(routes))
	for _, route := range routes {
		if isRouteFiltered(route) {
			continue
		}
		cleanRoutes = append(cleanRoutes, route)
	}

	data["routes"] = cleanRoutes

	return data, fromCache
}

func RoutesProtoCount(protocol string) (Parsed, bool) {
	cmd := routeQueryForChannel("route protocol "+protocol) + " count"
	return RunAndParse(cmd, parseRoutes)
}

func RoutesFiltered(protocol string) (Parsed, bool) {
	// Get all routes
	data, fromCache := RoutesProtoAll(protocol)

	routes, ok := data["routes"].([]interface{})
	if !ok {
		return NilParse, false
	}

	// Remove all unfiltered routes
	cleanRoutes := make([]interface{}, 0, len(routes))
	for _, route := range routes {
		if !isRouteFiltered(route) {
			continue
		}
		cleanRoutes = append(cleanRoutes, route)
	}

	data["routes"] = cleanRoutes

	return data, fromCache
}

func RoutesExport(protocol string) (Parsed, bool) {
	cmd := routeQueryForChannel("route all export " + protocol)
	return RunAndParse(cmd, parseRoutes)
}

func RoutesNoExport(protocol string) (Parsed, bool) {

	// In case we have a multi table setup, we have to query
	// the pipe protocol.
	if ParserConf.PerPeerTables &&
		strings.HasPrefix(protocol, ParserConf.PeerProtocolPrefix) {

		// Replace prefix
		protocol = ParserConf.PipeProtocolPrefix +
			protocol[len(ParserConf.PeerProtocolPrefix):]
	}

	cmd := routeQueryForChannel("route noexport '" + protocol + "' all")
	return RunAndParse(cmd, parseRoutes)
}

func RoutesExportCount(protocol string) (Parsed, bool) {
	cmd := routeQueryForChannel("route export "+protocol) + " count"
	return RunAndParse(cmd, parseRoutesCount)
}

func RoutesTable(table string) (Parsed, bool) {
	return RunAndParse("route table '"+table+"' all", parseRoutes)
}

func RoutesTableCount(table string) (Parsed, bool) {
	return RunAndParse("route table '"+table+"' count", parseRoutesCount)
}

func RoutesLookupTable(net string, table string) (Parsed, bool) {
	return RunAndParse("route for '"+net+"' table '"+table+"' all", parseRoutes)
}

func RoutesLookupProtocol(net string, protocol string) (Parsed, bool) {
	return RunAndParse("route for '"+net+"' protocol '"+protocol+"' all", parseRoutes)
}

func RoutesPeer(peer string) (Parsed, bool) {
	cmd := routeQueryForChannel("route export " + peer)
	return RunAndParse(cmd, parseRoutes)
}

func RoutesDump() (Parsed, bool) {
	if ParserConf.PerPeerTables {
		return RoutesDumpPerPeerTable()
	}

	return RoutesDumpSingleTable()
}

func RoutesDumpSingleTable() (Parsed, bool) {
	importedRes, cached := RunAndParse(routeQueryForChannel("route all"), parseRoutes)
	filteredRes, _ := RunAndParse(routeQueryForChannel("route all filtered"), parseRoutes)

	imported := importedRes["routes"]
	filtered := filteredRes["routes"]

	result := Parsed{
		"imported": imported,
		"filtered": filtered,
	}

	return result, cached
}

func RoutesDumpPerPeerTable() (Parsed, bool) {
	importedRes, cached := RunAndParse(routeQueryForChannel("route all"), parseRoutes)
	imported := importedRes["routes"]
	filtered := []Parsed{}

	// Get protocols with filtered routes
	protocolsRes, _ := ProtocolsBgp()
	protocols := protocolsRes["protocols"].(Parsed)

	for protocol, details := range protocols {
		details, ok := details.(Parsed)
		if !ok {
			continue
		}
		counters, ok := details["routes"].(Parsed)
		if !ok {
			continue
		}
		filterCount := counters["filtered"]
		if filterCount == 0 {
			continue // nothing to do here.
		}
		// Lookup filtered routes
		pfilteredRes, _ := RoutesFiltered(protocol)

		pfiltered, ok := pfilteredRes["routes"].([]Parsed)
		if !ok {
			continue // something went wrong...
		}

		filtered = append(filtered, pfiltered...)
	}

	result := Parsed{
		"imported": imported,
		"filtered": filtered,
	}

	return result, cached
}

func routeQueryForChannel(cmd string) string {
	status, _ := Status()
	birdStatus, ok := status["status"].(Parsed)
	if !ok {
		return cmd
	}

	version, ok := birdStatus["version"].(string)
	if !ok {
		return cmd
	}

	v, err := strconv.Atoi(string(version[0]))
	if err != nil || v <= 2 {
		return cmd
	}

	return cmd + " where net.type = NET_IP" + IPVersion
}
