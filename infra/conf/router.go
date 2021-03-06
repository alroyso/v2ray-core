package conf

import (
	"encoding/json"
	"strconv"
	"strings"

	"v2ray.com/core/app/router"
	"v2ray.com/core/common/net"
	"v2ray.com/core/common/platform/filesystem"

	"github.com/golang/protobuf/proto"
)

var localIPs = []string{
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
}

type RouterRulesConfig struct {
	RuleList       []json.RawMessage `json:"rules"`
	DomainStrategy string            `json:"domainStrategy"`
}

type BalancingRule struct {
	Tag       string     `json:"tag"`
	Selectors StringList `json:"selector"`

	Strategy      string `json:"strategy"`
	TotalMeasures uint32 `json:"totalMeasures"`
	Interval      uint32 `json:"interval"`
	Delay         uint32 `json:"delay"`
	Timeout       uint32 `json:"timeout"`
	Tolerance     uint32 `json:"tolerance"`
	ProbeTarget   string `json:"probeTarget"`
	ProbeContent  string `json:"probeContent"`
}

func splitCustomGeoipRules(rule *router.RoutingRule) []*router.RoutingRule {
	rules := make([]*router.RoutingRule, 0)

	// Create two new routing rules from existing one.
	geoip := new(router.RoutingRule)
	customGeoip := new(router.RoutingRule)
	*geoip = *rule
	*customGeoip = *rule

	// Null the unused one.
	geoip.CustomGeoip = geoip.CustomGeoip[:0]
	customGeoip.Geoip = customGeoip.Geoip[:0]

	rules = append(rules, geoip)
	rules = append(rules, customGeoip)

	return rules
}

func (r *BalancingRule) Build() (*router.BalancingRule, error) {
	if len(r.Tag) == 0 {
		return nil, newError("empty balancer tag")
	}
	if len(r.Selectors) == 0 {
		return nil, newError("empty selector list")
	}
	bs := router.BalancingRule_Random
	if r.Strategy == "latency" {
		bs = router.BalancingRule_Latency
	}
	totalMeasures := r.TotalMeasures
	interval := r.Interval
	delay := r.Delay
	timeout := r.Timeout
	tolerance := r.Tolerance
	probeTarget := r.ProbeTarget
	probeContent := r.ProbeContent
	if totalMeasures == 0 {
		totalMeasures = 3
	}
	if interval == 0 {
		interval = 120
	}
	if delay == 0 {
		delay = 1
	}
	if timeout == 0 {
		timeout = 5
	}
	if tolerance == 0 {
		tolerance = 300
	}
	if len(probeTarget) == 0 {
		probeTarget = "tls:www.google.com:443"
	}
	if len(probeContent) == 0 {
		probeContent = "HEAD / HTTP/1.1\r\n\r\n"
	}

	return &router.BalancingRule{
		Tag:               r.Tag,
		OutboundSelector:  []string(r.Selectors),
		BalancingStrategy: bs,
		TotalMeasures:     totalMeasures,
		Interval:          interval,
		Delay:             delay,
		Timeout:           timeout,
		Tolerance:         tolerance,
		ProbeTarget:       probeTarget,
		ProbeContent:      probeContent,
	}, nil
}

type RouterConfig struct {
	Settings       *RouterRulesConfig `json:"settings"` // Deprecated
	RuleList       []json.RawMessage  `json:"rules"`
	DomainStrategy *string            `json:"domainStrategy"`
	Balancers      []*BalancingRule   `json:"balancers"`
}

func (c *RouterConfig) getDomainStrategy() router.Config_DomainStrategy {
	ds := ""
	if c.DomainStrategy != nil {
		ds = *c.DomainStrategy
	} else if c.Settings != nil {
		ds = c.Settings.DomainStrategy
	}

	switch strings.ToLower(ds) {
	case "alwaysip":
		return router.Config_UseIp
	case "ipifnonmatch":
		return router.Config_IpIfNonMatch
	case "ipondemand":
		return router.Config_IpOnDemand
	default:
		return router.Config_AsIs
	}
}

func (c *RouterConfig) Build() (*router.Config, error) {
	config := new(router.Config)
	config.DomainStrategy = c.getDomainStrategy()

	rawRuleList := c.RuleList
	if c.Settings != nil {
		rawRuleList = append(c.RuleList, c.Settings.RuleList...)
	}
	for _, rawRule := range rawRuleList {
		rule, err := ParseRule(rawRule)
		if err != nil {
			return nil, err
		}
		if len(rule.Geoip) > 0 && len(rule.CustomGeoip) > 0 {
			rules := splitCustomGeoipRules(rule)
			config.Rule = append(config.Rule, rules...)
		} else {
			config.Rule = append(config.Rule, rule)
		}
	}
	for _, rawBalancer := range c.Balancers {
		balancer, err := rawBalancer.Build()
		if err != nil {
			return nil, err
		}
		config.BalancingRule = append(config.BalancingRule, balancer)
	}
	return config, nil
}

type RouterRule struct {
	Type        string `json:"type"`
	OutboundTag string `json:"outboundTag"`
	BalancerTag string `json:"balancerTag"`
}

func ParseIP(s string) (*router.CIDR, error) {
	var addr, mask string
	i := strings.Index(s, "/")
	if i < 0 {
		addr = s
	} else {
		addr = s[:i]
		mask = s[i+1:]
	}
	ip := net.ParseAddress(addr)
	switch ip.Family() {
	case net.AddressFamilyIPv4:
		bits := uint32(32)
		if len(mask) > 0 {
			bits64, err := strconv.ParseUint(mask, 10, 32)
			if err != nil {
				return nil, newError("invalid network mask for router: ", mask).Base(err)
			}
			bits = uint32(bits64)
		}
		if bits > 32 {
			return nil, newError("invalid network mask for router: ", bits)
		}
		return &router.CIDR{
			Ip:     []byte(ip.IP()),
			Prefix: bits,
		}, nil
	case net.AddressFamilyIPv6:
		bits := uint32(128)
		if len(mask) > 0 {
			bits64, err := strconv.ParseUint(mask, 10, 32)
			if err != nil {
				return nil, newError("invalid network mask for router: ", mask).Base(err)
			}
			bits = uint32(bits64)
		}
		if bits > 128 {
			return nil, newError("invalid network mask for router: ", bits)
		}
		return &router.CIDR{
			Ip:     []byte(ip.IP()),
			Prefix: bits,
		}, nil
	default:
		return nil, newError("unsupported address for router: ", s)
	}
}

func loadGeoIP(country string) ([]*router.CIDR, error) {
	return loadIP("geoip.dat", country)
}

func loadIP(filename, country string) ([]*router.CIDR, error) {
	geoipBytes, err := filesystem.ReadAsset(filename)
	if err != nil {
		return nil, newError("failed to open file: ", filename).Base(err)
	}
	var geoipList router.GeoIPList
	if err := proto.Unmarshal(geoipBytes, &geoipList); err != nil {
		return nil, err
	}

	for _, geoip := range geoipList.Entry {
		if geoip.CountryCode == country {
			return geoip.Cidr, nil
		}
	}

	return nil, newError("country not found: " + country)
}

func loadSite(filename, country string) ([]*router.Domain, error) {
	geositeBytes, err := filesystem.ReadAsset(filename)
	if err != nil {
		return nil, newError("failed to open file: ", filename).Base(err)
	}
	var geositeList router.GeoSiteList
	if err := proto.Unmarshal(geositeBytes, &geositeList); err != nil {
		return nil, err
	}

	for _, site := range geositeList.Entry {
		if site.CountryCode == country {
			return site.Domain, nil
		}
	}

	return nil, newError("country not found: " + country)
}

type AttributeMatcher interface {
	Match(*router.Domain) bool
}

type BooleanMatcher string

func (m BooleanMatcher) Match(domain *router.Domain) bool {
	for _, attr := range domain.Attribute {
		if attr.Key == string(m) {
			return true
		}
	}
	return false
}

type AttributeList struct {
	matcher []AttributeMatcher
}

func (al *AttributeList) Match(domain *router.Domain) bool {
	for _, matcher := range al.matcher {
		if !matcher.Match(domain) {
			return false
		}
	}
	return true
}

func (al *AttributeList) IsEmpty() bool {
	return len(al.matcher) == 0
}

func parseAttrs(attrs []string) *AttributeList {
	al := new(AttributeList)
	for _, attr := range attrs {
		lc := strings.ToLower(attr)
		al.matcher = append(al.matcher, BooleanMatcher(lc))
	}
	return al
}

func loadGeositeWithAttr(file string, siteWithAttr string) ([]*router.Domain, error) {
	parts := strings.Split(siteWithAttr, "@")
	if len(parts) == 0 {
		return nil, newError("empty site")
	}
	country := strings.ToUpper(parts[0])
	attrs := parseAttrs(parts[1:])
	domains, err := loadSite(file, country)
	if err != nil {
		return nil, err
	}

	if attrs.IsEmpty() {
		return domains, nil
	}

	filteredDomains := make([]*router.Domain, 0, len(domains))
	for _, domain := range domains {
		if attrs.Match(domain) {
			filteredDomains = append(filteredDomains, domain)
		}
	}

	return filteredDomains, nil
}

func parseDomainRule(domain string) ([]*router.Domain, error) {
	if strings.HasPrefix(domain, "geosite:") {
		country := strings.ToUpper(domain[8:])
		domains, err := loadGeositeWithAttr("geosite.dat", country)
		if err != nil {
			return nil, newError("failed to load geosite: ", country).Base(err)
		}
		return domains, nil
	}

	if strings.HasPrefix(domain, "ext:") {
		kv := strings.Split(domain[4:], ":")
		if len(kv) != 2 {
			return nil, newError("invalid external resource: ", domain)
		}
		filename := kv[0]
		country := kv[1]
		domains, err := loadGeositeWithAttr(filename, country)
		if err != nil {
			return nil, newError("failed to load external sites: ", country, " from ", filename).Base(err)
		}
		return domains, nil
	}

	domainRule := new(router.Domain)
	switch {
	case strings.HasPrefix(domain, "regexp:"):
		domainRule.Type = router.Domain_Regex
		domainRule.Value = domain[7:]
	case strings.HasPrefix(domain, "domain:"):
		domainRule.Type = router.Domain_Domain
		domainRule.Value = domain[7:]
	case strings.HasPrefix(domain, "full:"):
		domainRule.Type = router.Domain_Full
		domainRule.Value = domain[5:]
	case strings.HasPrefix(domain, "keyword:"):
		domainRule.Type = router.Domain_Plain
		domainRule.Value = domain[8:]
	default:
		domainRule.Type = router.Domain_Plain
		domainRule.Value = domain
	}
	return []*router.Domain{domainRule}, nil
}

func toCidrList(ips StringList) ([]*router.GeoIP, []string, error) {
	var geoipList []*router.GeoIP
	var customCidrs []*router.CIDR
	var customGeoipList []string

	for _, ip := range ips {
		if strings.HasPrefix(strings.ToLower(ip), "geoip:private") {
			for _, ip := range localIPs {
				ipRule, err := ParseIP(ip)
				if err != nil {
					return nil, nil, newError("invalid IP: ", ip).Base(err)
				}
				customCidrs = append(customCidrs, ipRule)
			}
			continue
		}

		if strings.HasPrefix(ip, "geoip:") {
			country := ip[6:]
			customGeoipList = append(customGeoipList, strings.ToUpper(country))
			// country := ip[6:]
			// geoip, err := loadGeoIP(strings.ToUpper(country))
			// if err != nil {
			// 	return nil, newError("failed to load GeoIP: ", country).Base(err)
			// }

			// geoipList = append(geoipList, &router.GeoIP{
			// 	CountryCode: strings.ToUpper(country),
			// 	Cidr:        geoip,
			// })
			continue
		}

		if strings.HasPrefix(ip, "ext:") {
			kv := strings.Split(ip[4:], ":")
			if len(kv) != 2 {
				return nil, nil, newError("invalid external resource: ", ip)
			}

			filename := kv[0]
			country := kv[1]
			geoip, err := loadIP(filename, strings.ToUpper(country))
			if err != nil {
				return nil, nil, newError("failed to load IPs: ", country, " from ", filename).Base(err)
			}

			geoipList = append(geoipList, &router.GeoIP{
				CountryCode: strings.ToUpper(filename + "_" + country),
				Cidr:        geoip,
			})

			continue
		}

		ipRule, err := ParseIP(ip)
		if err != nil {
			return nil, nil, newError("invalid IP: ", ip).Base(err)
		}
		customCidrs = append(customCidrs, ipRule)
	}

	if len(customCidrs) > 0 {
		geoipList = append(geoipList, &router.GeoIP{
			Cidr: customCidrs,
		})
	}

	return geoipList, customGeoipList, nil
}

func parseFieldRule(msg json.RawMessage) (*router.RoutingRule, error) {
	type RawFieldRule struct {
		RouterRule
		Domain      *StringList  `json:"domain"`
		IP          *StringList  `json:"ip"`
		Port        *PortList    `json:"port"`
		Network     *NetworkList `json:"network"`
		SourceIP    *StringList  `json:"source"`
		User        *StringList  `json:"user"`
		InboundTag  *StringList  `json:"inboundTag"`
		Protocols   *StringList  `json:"protocol"`
		Attributes  string       `json:"attrs"`
		Application *StringList  `json:"app"`
	}
	rawFieldRule := new(RawFieldRule)
	err := json.Unmarshal(msg, rawFieldRule)
	if err != nil {
		return nil, err
	}

	rule := new(router.RoutingRule)
	if len(rawFieldRule.OutboundTag) > 0 {
		rule.TargetTag = &router.RoutingRule_Tag{
			Tag: rawFieldRule.OutboundTag,
		}
	} else if len(rawFieldRule.BalancerTag) > 0 {
		rule.TargetTag = &router.RoutingRule_BalancingTag{
			BalancingTag: rawFieldRule.BalancerTag,
		}
	} else {
		return nil, newError("neither outboundTag nor balancerTag is specified in routing rule")
	}

	if rawFieldRule.Domain != nil {
		for _, domain := range *rawFieldRule.Domain {
			rules, err := parseDomainRule(domain)
			if err != nil {
				return nil, newError("failed to parse domain rule: ", domain).Base(err)
			}
			rule.Domain = append(rule.Domain, rules...)
		}
	}

	if rawFieldRule.IP != nil {
		geoipList, customGeoipList, err := toCidrList(*rawFieldRule.IP)
		if err != nil {
			return nil, err
		}
		rule.CustomGeoip = customGeoipList
		rule.Geoip = geoipList
	}

	if rawFieldRule.Port != nil {
		rule.PortList = rawFieldRule.Port.Build()
	}

	if rawFieldRule.Network != nil {
		rule.Networks = rawFieldRule.Network.Build()
	}

	if rawFieldRule.SourceIP != nil {
		geoipList, _, err := toCidrList(*rawFieldRule.SourceIP)
		if err != nil {
			return nil, err
		}
		rule.SourceGeoip = geoipList
	}

	if rawFieldRule.User != nil {
		for _, s := range *rawFieldRule.User {
			rule.UserEmail = append(rule.UserEmail, s)
		}
	}

	if rawFieldRule.InboundTag != nil {
		for _, s := range *rawFieldRule.InboundTag {
			rule.InboundTag = append(rule.InboundTag, s)
		}
	}

	if rawFieldRule.Protocols != nil {
		for _, s := range *rawFieldRule.Protocols {
			rule.Protocol = append(rule.Protocol, s)
		}
	}

	if len(rawFieldRule.Attributes) > 0 {
		rule.Attributes = rawFieldRule.Attributes
	}

	if rawFieldRule.Application != nil {
		for _, s := range *rawFieldRule.Application {
			rule.Application = append(rule.Application, s)
		}
	}

	return rule, nil
}

func ParseRule(msg json.RawMessage) (*router.RoutingRule, error) {
	rawRule := new(RouterRule)
	err := json.Unmarshal(msg, rawRule)
	if err != nil {
		return nil, newError("invalid router rule").Base(err)
	}
	if rawRule.Type == "field" || len(rawRule.Type) == 0 {
		fieldrule, err := parseFieldRule(msg)
		if err != nil {
			return nil, newError("invalid field rule").Base(err)
		}
		return fieldrule, nil
	}
	if rawRule.Type == "chinaip" {
		return &router.RoutingRule{
			TargetTag: &router.RoutingRule_Tag{
				Tag: rawRule.OutboundTag,
			},
			CustomGeoip: []string{"CN"},
		}, nil
		// chinaiprule, err := parseChinaIPRule(msg)
		// if err != nil {
		// 	return nil, newError("invalid chinaip rule").Base(err)
		// }
		// return chinaiprule, nil
	}
	if rawRule.Type == "chinasites" {
		chinasitesrule, err := parseChinaSitesRule(msg)
		if err != nil {
			return nil, newError("invalid chinasites rule").Base(err)
		}
		return chinasitesrule, nil
	}
	return nil, newError("unknown router rule type: ", rawRule.Type)
}

func parseChinaIPRule(data []byte) (*router.RoutingRule, error) {
	rawRule := new(RouterRule)
	err := json.Unmarshal(data, rawRule)
	if err != nil {
		return nil, newError("invalid router rule").Base(err)
	}
	chinaIPs, err := loadGeoIP("CN")
	if err != nil {
		return nil, newError("failed to load geoip:cn").Base(err)
	}
	return &router.RoutingRule{
		TargetTag: &router.RoutingRule_Tag{
			Tag: rawRule.OutboundTag,
		},
		Cidr: chinaIPs,
	}, nil
}

func parseChinaSitesRule(data []byte) (*router.RoutingRule, error) {
	rawRule := new(RouterRule)
	err := json.Unmarshal(data, rawRule)
	if err != nil {
		return nil, newError("invalid router rule").Base(err).AtError()
	}
	domains, err := loadGeositeWithAttr("geosite.dat", "CN")
	if err != nil {
		return nil, newError("failed to load geosite:cn.").Base(err)
	}
	return &router.RoutingRule{
		TargetTag: &router.RoutingRule_Tag{
			Tag: rawRule.OutboundTag,
		},
		Domain: domains,
	}, nil
}
