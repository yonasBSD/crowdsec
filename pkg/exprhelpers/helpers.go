package exprhelpers

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bluele/gcache"
	"github.com/cespare/xxhash/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/expr-lang/expr"
	"github.com/oschwald/geoip2-golang"
	"github.com/oschwald/maxminddb-golang"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/umahmood/haversine"
	"github.com/wasilibs/go-re2"

	"github.com/crowdsecurity/go-cs-lib/cstime"

	"github.com/crowdsecurity/crowdsec/pkg/cache"
	"github.com/crowdsecurity/crowdsec/pkg/database"
	"github.com/crowdsecurity/crowdsec/pkg/fflag"
	"github.com/crowdsecurity/crowdsec/pkg/metrics"
	"github.com/crowdsecurity/crowdsec/pkg/types"
)

var (
	dataFile      map[string][]string
	dataFileRegex map[string][]*regexp.Regexp
	dataFileRe2   map[string][]*re2.Regexp
)

// This is used to (optionally) cache regexp results for RegexpInFile operations
var dataFileRegexCache map[string]gcache.Cache = make(map[string]gcache.Cache)

var dbClient *database.Client

var exprFunctionOptions []expr.Option

func init() { //nolint:gochecknoinits
	exprFunctionOptions = make([]expr.Option, len(exprFuncs))
	for i, fn := range exprFuncs {
		exprFunctionOptions[i] = expr.Function(fn.name, fn.function, fn.signature...)
	}
}

var keyValuePattern = regexp.MustCompile(`(?P<key>[^=\s]+)=(?:"(?P<quoted_value>[^"\\]*(?:\\.[^"\\]*)*)"|(?P<value>[^=\s]+)|\s*)`)

var (
	geoIPCityReader  *geoip2.Reader
	geoIPASNReader   *geoip2.Reader
	geoIPRangeReader *maxminddb.Reader
)

func GetExprOptions(ctx map[string]any) []expr.Option {
	// copy the pre‑built options + one Env(...) for this call
	opts := make([]expr.Option, len(exprFunctionOptions)+1)
	copy(opts, exprFunctionOptions)
	opts[len(opts)-1] = expr.Env(ctx)

	return opts
}

func GeoIPInit(datadir string) error {
	var err error

	geoIPCityReader, err = geoip2.Open(filepath.Join(datadir, "GeoLite2-City.mmdb"))
	if err != nil {
		log.Errorf("unable to open GeoLite2-City.mmdb : %s", err)
		return err
	}

	geoIPASNReader, err = geoip2.Open(filepath.Join(datadir, "GeoLite2-ASN.mmdb"))
	if err != nil {
		log.Errorf("unable to open GeoLite2-ASN.mmdb : %s", err)
		return err
	}

	geoIPRangeReader, err = maxminddb.Open(filepath.Join(datadir, "GeoLite2-ASN.mmdb"))
	if err != nil {
		log.Errorf("unable to open GeoLite2-ASN.mmdb : %s", err)
		return err
	}

	return nil
}

func GeoIPClose() {
	if geoIPCityReader != nil {
		geoIPCityReader.Close()
	}

	if geoIPASNReader != nil {
		geoIPASNReader.Close()
	}

	if geoIPRangeReader != nil {
		geoIPRangeReader.Close()
	}
}

func Init(databaseClient *database.Client) error {
	dataFile = make(map[string][]string)
	dataFileRegex = make(map[string][]*regexp.Regexp)
	dataFileRe2 = make(map[string][]*re2.Regexp)
	dbClient = databaseClient

	XMLCacheInit()

	return nil
}

// ResetDataFiles clears all datafile-related global variables.
// This should be called during HUP reload to ensure clean state.
func ResetDataFiles() {
	dataFile = make(map[string][]string)
	dataFileRegex = make(map[string][]*regexp.Regexp)
	dataFileRe2 = make(map[string][]*re2.Regexp)
	dataFileRegexCache = make(map[string]gcache.Cache)
}

func RegexpCacheInit(filename string, cacheCfg types.DataSource) error {
	// cache is explicitly disabled
	if cacheCfg.Cache != nil && !*cacheCfg.Cache {
		return nil
	}
	// cache is implicitly disabled if no cache config is provided
	if cacheCfg.Strategy == nil && cacheCfg.TTL == nil && cacheCfg.Size == nil {
		return nil
	}
	// cache is enabled

	size := 50
	if cacheCfg.Size != nil {
		size = *cacheCfg.Size
	}

	gc := gcache.New(size)

	strategy := "LRU"
	if cacheCfg.Strategy != nil {
		strategy = *cacheCfg.Strategy
	}

	switch strategy {
	case "LRU":
		gc = gc.LRU()
	case "LFU":
		gc = gc.LFU()
	case "ARC":
		gc = gc.ARC()
	default:
		return fmt.Errorf("unknown cache strategy '%s'", strategy)
	}

	if cacheCfg.TTL != nil {
		gc.Expiration(*cacheCfg.TTL)
	}

	cache := gc.Build()
	dataFileRegexCache[filename] = cache

	return nil
}

// UpdateCacheMetrics is called directly by the prom handler
func UpdateRegexpCacheMetrics() {
	metrics.RegexpCacheMetrics.Reset()

	for name := range dataFileRegexCache {
		metrics.RegexpCacheMetrics.With(prometheus.Labels{"name": name}).Set(float64(dataFileRegexCache[name].Len(true)))
	}
}

func FileInit(directory string, filename string, fileType string) error {
	log.Debugf("init (folder:%s) (file:%s) (type:%s)", directory, filename, fileType)

	if fileType == "" {
		log.Debugf("ignored file %s%s because no type specified", directory, filename)
		return nil
	}

	ok, err := existsInFileMaps(filename, fileType)
	if ok {
		log.Debugf("ignored file %s%s because already loaded", directory, filename)
		return nil
	}

	if err != nil {
		return err
	}

	file, err := os.Open(filepath.Join(directory, filename))
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "#") { // allow comments
			continue
		}

		if scanner.Text() == "" { // skip empty lines
			continue
		}

		switch fileType {
		case "regex", "regexp":
			if fflag.Re2RegexpInfileSupport.IsEnabled() {
				dataFileRe2[filename] = append(dataFileRe2[filename], re2.MustCompile(scanner.Text()))
				continue
			}

			dataFileRegex[filename] = append(dataFileRegex[filename], regexp.MustCompile(scanner.Text()))
		case "string":
			dataFile[filename] = append(dataFile[filename], scanner.Text())
		}
	}

	return scanner.Err()
}

// Expr helpers

func Distinct(params ...any) (any, error) {
	if rt := reflect.TypeOf(params[0]).Kind(); rt != reflect.Slice && rt != reflect.Array {
		return nil, nil
	}

	array := params[0].([]any)
	if array == nil {
		return []any{}, nil
	}

	exists := make(map[any]bool)
	ret := make([]any, 0)

	for _, val := range array {
		if _, ok := exists[val]; !ok {
			exists[val] = true
			ret = append(ret, val)
		}
	}

	return ret, nil
}

func FlattenDistinct(params ...any) (any, error) {
	return Distinct(flatten(nil, reflect.ValueOf(params)))
}

func Flatten(params ...any) (any, error) {
	return flatten(nil, reflect.ValueOf(params)), nil
}

func flatten(args []any, v reflect.Value) []any {
	if v.Kind() == reflect.Interface {
		v = v.Elem()
	}

	if v.Kind() == reflect.Array || v.Kind() == reflect.Slice {
		for i := range v.Len() {
			args = flatten(args, v.Index(i))
		}
	} else {
		args = append(args, v.Interface())
	}

	return args
}

func existsInFileMaps(filename string, ftype string) (bool, error) {
	var err error

	ok := false

	switch ftype {
	case "regex", "regexp":
		if fflag.Re2RegexpInfileSupport.IsEnabled() {
			_, ok = dataFileRe2[filename]
		} else {
			_, ok = dataFileRegex[filename]
		}
	case "string":
		_, ok = dataFile[filename]
	default:
		err = fmt.Errorf("unknown data type '%s' for : '%s'", ftype, filename)
	}

	return ok, err
}

// Expr helpers

// func Get(arr []string, index int) string {
func Get(params ...any) (any, error) {
	arr := params[0].([]string)
	index := params[1].(int)

	if index >= len(arr) {
		return "", nil
	}

	return arr[index], nil
}

// func Atof(x string) float64 {
func Atof(params ...any) (any, error) {
	x := params[0].(string)
	log.Debugf("debug atof %s", x)

	ret, err := strconv.ParseFloat(x, 64)
	if err != nil {
		log.Warningf("Atof : can't convert float '%s' : %v", x, err)
	}

	return ret, nil
}

// func Upper(s string) string {
func Upper(params ...any) (any, error) {
	s := params[0].(string)
	return strings.ToUpper(s), nil
}

// func Lower(s string) string {
func Lower(params ...any) (any, error) {
	s := params[0].(string)
	return strings.ToLower(s), nil
}

// func Distance(lat1 string, long1 string, lat2 string, long2 string) (float64, error) {
func Distance(params ...any) (any, error) {
	lat1 := params[0].(string)
	long1 := params[1].(string)
	lat2 := params[2].(string)
	long2 := params[3].(string)

	lat1f, err := strconv.ParseFloat(lat1, 64)
	if err != nil {
		log.Warningf("lat1 is not a float : %v", err)

		return 0.0, fmt.Errorf("lat1 is not a float : %v", err)
	}

	long1f, err := strconv.ParseFloat(long1, 64)
	if err != nil {
		log.Warningf("long1 is not a float : %v", err)

		return 0.0, fmt.Errorf("long1 is not a float : %v", err)
	}

	lat2f, err := strconv.ParseFloat(lat2, 64)
	if err != nil {
		log.Warningf("lat2 is not a float : %v", err)

		return 0.0, fmt.Errorf("lat2 is not a float : %v", err)
	}

	long2f, err := strconv.ParseFloat(long2, 64)
	if err != nil {
		log.Warningf("long2 is not a float : %v", err)

		return 0.0, fmt.Errorf("long2 is not a float : %v", err)
	}

	// either set of coordinates is 0,0, return 0 to avoid FPs
	if (lat1f == 0.0 && long1f == 0.0) || (lat2f == 0.0 && long2f == 0.0) {
		log.Warningf("one of the coordinates is 0,0, returning 0")
		return 0.0, nil
	}

	first := haversine.Coord{Lat: lat1f, Lon: long1f}
	second := haversine.Coord{Lat: lat2f, Lon: long2f}

	_, km := haversine.Distance(first, second)

	return km, nil
}

// func QueryEscape(s string) string {
func QueryEscape(params ...any) (any, error) {
	s := params[0].(string)
	return url.QueryEscape(s), nil
}

// func PathEscape(s string) string {
func PathEscape(params ...any) (any, error) {
	s := params[0].(string)
	return url.PathEscape(s), nil
}

// func PathUnescape(s string) string {
func PathUnescape(params ...any) (any, error) {
	s := params[0].(string)

	ret, err := url.PathUnescape(s)
	if err != nil {
		log.Debugf("unable to PathUnescape '%s': %+v", s, err)
		return s, nil
	}

	return ret, nil
}

// func QueryUnescape(s string) string {
func QueryUnescape(params ...any) (any, error) {
	s := params[0].(string)

	ret, err := url.QueryUnescape(s)
	if err != nil {
		log.Debugf("unable to QueryUnescape '%s': %+v", s, err)
		return s, nil
	}

	return ret, nil
}

// func File(filename string) []string {
func File(params ...any) (any, error) {
	filename := params[0].(string)
	if _, ok := dataFile[filename]; ok {
		return dataFile[filename], nil
	}

	log.Errorf("file '%s' (type:string) not found in expr library", filename)
	log.Errorf("expr library : %s", spew.Sdump(dataFile))

	return []string{}, nil
}

// func RegexpInFile(data string, filename string) bool {
func RegexpInFile(params ...any) (any, error) {
	data := params[0].(string)
	filename := params[1].(string)

	var hash uint64

	hasCache := false
	matched := false

	if _, ok := dataFileRegexCache[filename]; ok {
		hasCache = true
		hash = xxhash.Sum64String(data)

		if val, err := dataFileRegexCache[filename].Get(hash); err == nil {
			return val.(bool), nil
		}
	}

	switch fflag.Re2RegexpInfileSupport.IsEnabled() {
	case true:
		if _, ok := dataFileRe2[filename]; ok {
			for _, re := range dataFileRe2[filename] {
				if re.MatchString(data) {
					matched = true
					break
				}
			}
		} else {
			log.Errorf("file '%s' (type:regexp) not found in expr library", filename)
			log.Errorf("expr library : %s", spew.Sdump(dataFileRe2))
		}
	case false:
		if _, ok := dataFileRegex[filename]; ok {
			for _, re := range dataFileRegex[filename] {
				if re.MatchString(data) {
					matched = true
					break
				}
			}
		} else {
			log.Errorf("file '%s' (type:regexp) not found in expr library", filename)
			log.Errorf("expr library : %s", spew.Sdump(dataFileRegex))
		}
	}

	if hasCache {
		dataFileRegexCache[filename].Set(hash, matched)
	}

	return matched, nil
}

// func IpInRange(ip string, ipRange string) bool {
func IpInRange(params ...any) (any, error) {
	var (
		err           error
		ipParsed      net.IP
		ipRangeParsed *net.IPNet
	)

	ip := params[0].(string)
	ipRange := params[1].(string)

	ipParsed = net.ParseIP(ip)
	if ipParsed == nil {
		log.Debugf("'%s' is not a valid IP", ip)
		return false, nil
	}

	if _, ipRangeParsed, err = net.ParseCIDR(ipRange); err != nil {
		log.Debugf("'%s' is not a valid IP Range", ipRange)
		return false, nil //nolint:nilerr // This helper did not return an error before the move to expr.Function, we keep this behavior for backward compatibility
	}

	if ipRangeParsed.Contains(ipParsed) {
		return true, nil
	}

	return false, nil
}

// func IsIPV6(ip string) bool {
func IsIPV6(params ...any) (any, error) {
	ip := params[0].(string)
	ipParsed := net.ParseIP(ip)

	if ipParsed == nil {
		log.Debugf("'%s' is not a valid IP", ip)
		return false, nil
	}

	// If it's a valid IP and can't be converted to IPv4 then it is an IPv6
	return ipParsed.To4() == nil, nil
}

// func IsIPV4(ip string) bool {
func IsIPV4(params ...any) (any, error) {
	ip := params[0].(string)
	ipParsed := net.ParseIP(ip)

	if ipParsed == nil {
		log.Debugf("'%s' is not a valid IP", ip)
		return false, nil
	}

	return ipParsed.To4() != nil, nil
}

// func IsIP(ip string) bool {
func IsIP(params ...any) (any, error) {
	ip := params[0].(string)
	ipParsed := net.ParseIP(ip)

	if ipParsed == nil {
		log.Debugf("'%s' is not a valid IP", ip)
		return false, nil
	}

	return true, nil
}

// func IpToRange(ip string, cidr string) string {
func IpToRange(params ...any) (any, error) {
	ip := params[0].(string)
	cidr := params[1].(string)
	cidr = strings.TrimPrefix(cidr, "/")

	mask, err := strconv.Atoi(cidr)
	if err != nil {
		log.Errorf("bad cidr '%s': %s", cidr, err)
		return "", nil
	}

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		log.Errorf("can't parse IP address '%s': %v", ip, err)
		return "", nil
	}

	prefix, err := addr.Prefix(mask)
	if err != nil {
		log.Errorf("can't create prefix from IP address '%s' and mask '%d': %v", ip, mask, err)
		return "", nil
	}

	return prefix.String(), nil
}

// func TimeNow() string {
func TimeNow(params ...any) (any, error) {
	return time.Now().UTC().Format(time.RFC3339), nil
}

// func ParseUri(uri string) map[string][]string {
func ParseUri(params ...any) (any, error) {
	uri := params[0].(string)
	ret := make(map[string][]string)

	u, err := url.Parse(uri)
	if err != nil {
		log.Errorf("Could not parse URI: %s", err)
		return ret, nil
	}

	parsed, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		log.Errorf("Could not parse query uri : %s", err)
		return ret, nil
	}

	maps.Copy(ret, parsed)

	return ret, nil
}

// func AverageInterval(times []time.Time) time.Duration
func AverageInterval(params ...any) (any, error) {
	if len(params) != 1 {
		return 0, errors.New("AverageInterval expects exactly one parameter: a slice of times")
	}

	var times []time.Time

	// Handle both []time.Time and []interface{} (from expr map function)
	switch v := params[0].(type) {
	case []time.Time:
		times = v
	case []interface{}:
		times = make([]time.Time, len(v))
		for i, item := range v {
			t, ok := item.(time.Time)
			if !ok {
				return 0, fmt.Errorf("element at index %d is not a time.Time", i)
			}
			times[i] = t
		}
	default:
		return 0, errors.New("AverageInterval expects a slice of times")
	}

	if len(times) < 2 {
		return 0, errors.New("need at least two times to calculate an average interval")
	}

	// Sort times in ascending order
	sort.Slice(times, func(i, j int) bool {
		return times[i].Before(times[j])
	})

	var total time.Duration
	for i := 1; i < len(times); i++ {
		total += times[i].Sub(times[i-1])
	}

	average := time.Duration(int64(total) / int64(len(times)-1))
	return average, nil
}

// func MedianInterval(times []time.Time) (time.Duration, error)
func MedianInterval(params ...any) (any, error) {
	if len(params) != 1 {
		return 0, errors.New("MedianInterval expects exactly one parameter: a slice of times")
	}

	var times []time.Time

	// Handle both []time.Time and []interface{} (from expr map function)
	switch v := params[0].(type) {
	case []time.Time:
		times = v
	case []interface{}:
		times = make([]time.Time, len(v))
		for i, item := range v {
			t, ok := item.(time.Time)
			if !ok {
				return 0, fmt.Errorf("element at index %d is not a time.Time", i)
			}
			times[i] = t
		}
	default:
		return 0, errors.New("MedianInterval expects a slice of times")
	}

	if len(times) < 2 {
		return 0, errors.New("need at least two times to calculate a median")
	}

	// Sort times
	sort.Slice(times, func(i, j int) bool {
		return times[i].Before(times[j])
	})

	// Compute intervals
	intervals := make([]time.Duration, len(times)-1)
	for i := 1; i < len(times); i++ {
		intervals[i-1] = times[i].Sub(times[i-1])
	}

	// Sort intervals for median calculation
	sort.Slice(intervals, func(i, j int) bool {
		return intervals[i] < intervals[j]
	})

	n := len(intervals)
	if n%2 == 1 {
		return intervals[n/2], nil
	}
	return (intervals[n/2-1] + intervals[n/2]) / 2, nil
}

// func KeyExists(key string, dict map[string]interface{}) bool {
func KeyExists(params ...any) (any, error) {
	key := params[0].(string)
	dict := params[1].(map[string]any)
	_, ok := dict[key]

	return ok, nil
}

// func GetDecisionsCount(value string) int {
func GetDecisionsCount(params ...any) (any, error) {
	value := params[0].(string)

	if dbClient == nil {
		log.Error("No database config to call GetDecisionsCount()")
		return 0, nil
	}

	ctx := context.TODO()

	count, err := dbClient.CountDecisionsByValue(ctx, value, nil, false)
	if err != nil {
		log.Errorf("Failed to get decisions count from value '%s'", value)
		return 0, nil //nolint:nilerr // This helper did not return an error before the move to expr.Function, we keep this behavior for backward compatibility
	}

	return count, nil
}

// func GetDecisionsSinceCount(value string, since string) int {
func GetDecisionsSinceCount(params ...any) (any, error) {
	value := params[0].(string)
	since := params[1].(string)

	if dbClient == nil {
		log.Error("No database config to call GetDecisionsSinceCount()")
		return 0, nil
	}

	sinceDuration, err := cstime.ParseDurationWithDays(since)
	if err != nil {
		log.Errorf("Failed to parse since parameter '%s' : %s", since, err)
		return 0, nil
	}

	ctx := context.TODO()
	sinceTime := time.Now().UTC().Add(-sinceDuration)

	count, err := dbClient.CountDecisionsByValue(ctx, value, &sinceTime, false)
	if err != nil {
		log.Errorf("Failed to get decisions count from value '%s'", value)
		return 0, nil //nolint:nilerr // This helper did not return an error before the move to expr.Function, we keep this behavior for backward compatibility
	}

	return count, nil
}

func GetActiveDecisionsCount(params ...any) (any, error) {
	value := params[0].(string)

	if dbClient == nil {
		log.Error("No database config to call GetActiveDecisionsCount()")
		return 0, nil
	}

	ctx := context.TODO()

	count, err := dbClient.CountDecisionsByValue(ctx, value, nil, true)
	if err != nil {
		log.Errorf("Failed to get active decisions count from value '%s'", value)
		return 0, err
	}

	return count, nil
}

func GetActiveDecisionsTimeLeft(params ...any) (any, error) {
	value := params[0].(string)

	if dbClient == nil {
		log.Error("No database config to call GetActiveDecisionsTimeLeft()")
		return 0, nil
	}

	ctx := context.TODO()

	timeLeft, err := dbClient.GetActiveDecisionsTimeLeftByValue(ctx, value)
	if err != nil {
		log.Errorf("Failed to get active decisions time left from value '%s'", value)
		return 0, err
	}

	return timeLeft, nil
}

// func LookupHost(value string) []string {
func LookupHost(params ...any) (any, error) {
	value := params[0].(string)

	addresses, err := net.LookupHost(value)
	if err != nil {
		log.Errorf("Failed to lookup host '%s' : %s", value, err)
		return []string{}, nil
	}

	return addresses, nil
}

// func ParseUnixTime(value string) (time.Time, error) {
func ParseUnixTime(params ...any) (any, error) {
	value := params[0].(string)
	// Splitting string here as some unix timestamp may have milliseconds and break ParseInt
	i, err := strconv.ParseInt(strings.Split(value, ".")[0], 10, 64)
	if err != nil || i <= 0 {
		return time.Time{}, fmt.Errorf("unable to parse %s as unix timestamp", value)
	}

	return time.Unix(i, 0), nil
}

// func ParseUnix(value string) string {
func ParseUnix(params ...any) (any, error) {
	value := params[0].(string)

	t, err := ParseUnixTime(value)
	if err != nil {
		log.Error(err)
		return "", nil
	}

	return t.(time.Time).Format(time.RFC3339), nil
}

// func ToString(value interface{}) string {
func ToString(params ...any) (any, error) {
	value := params[0]

	s, ok := value.(string)
	if !ok {
		return "", nil
	}

	return s, nil
}

// func GetFromStash(cacheName string, key string) (string, error) {
func GetFromStash(params ...any) (any, error) {
	cacheName := params[0].(string)
	key := params[1].(string)

	return cache.GetKey(cacheName, key)
}

// func SetInStash(cacheName string, key string, value string, expiration *time.Duration) any {
func SetInStash(params ...any) (any, error) {
	cacheName := params[0].(string)
	key := params[1].(string)
	value := params[2].(string)
	expiration := params[3].(*time.Duration)

	return cache.SetKey(cacheName, key, value, expiration), nil
}

func Sprintf(params ...any) (any, error) {
	format := params[0].(string)
	return fmt.Sprintf(format, params[1:]...), nil
}

// func Match(pattern, name string) bool {
func Match(params ...any) (any, error) {
	var matched bool

	pattern := params[0].(string)
	name := params[1].(string)

	if pattern == "" {
		return name == "", nil
	}

	if name == "" {
		if pattern == "*" || pattern == "" {
			return true, nil
		}

		return false, nil
	}

	if pattern[0] == '*' {
		for i := 0; i <= len(name); i++ {
			matched, _ := Match(pattern[1:], name[i:])
			if matched.(bool) {
				return matched, nil
			}
		}

		return matched, nil
	}

	if pattern[0] == '?' || pattern[0] == name[0] {
		return Match(pattern[1:], name[1:])
	}

	return matched, nil
}

func FloatApproxEqual(params ...any) (any, error) {
	float1 := params[0].(float64)
	float2 := params[1].(float64)

	if math.Abs(float1-float2) < 1e-6 {
		return true, nil
	}

	return false, nil
}

func B64Decode(params ...any) (any, error) {
	encoded := params[0].(string)

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}

	return string(decoded), nil
}

func ParseKV(params ...any) (any, error) {
	blob := params[0].(string)
	target := params[1].(map[string]any)
	prefix := params[2].(string)

	matches := keyValuePattern.FindAllStringSubmatch(blob, -1)
	if matches == nil {
		log.Errorf("could not find any key/value pair in line")
		return nil, errors.New("invalid input format")
	}

	if _, ok := target[prefix]; !ok {
		target[prefix] = make(map[string]string)
	} else {
		_, ok := target[prefix].(map[string]string)
		if !ok {
			log.Errorf("ParseKV: target is not a map[string]string")
			return nil, errors.New("target is not a map[string]string")
		}
	}

	for _, match := range matches {
		key := ""
		value := ""

		for i, name := range keyValuePattern.SubexpNames() {
			switch {
			case name == "key":
				key = match[i]
			case name == "quoted_value" && match[i] != "":
				value = match[i]
			case name == "value" && match[i] != "":
				value = match[i]
			}
		}

		target[prefix].(map[string]string)[key] = value
	}

	log.Tracef("unmarshaled KV: %+v", target[prefix])

	return nil, nil
}

func Hostname(params ...any) (any, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}

	return hostname, nil
}
