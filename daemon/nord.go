package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	// Used only when the TV has no usable system CA bundle.
	_ "golang.org/x/crypto/x509roots/fallback"
)

var nordAPI = envOr("NORRSKEN_API", "https://api.nordvpn.com")

var httpClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConns:        4,
	},
}

var errTokenRejected = errors.New("NordVPN rejected this access token")

func apiGet(ctx context.Context, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", nordAPI+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "norrsken/"+version)
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.SetBasicAuth("token", token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach NordVPN: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if token != "" && (resp.StatusCode == 401 || resp.StatusCode == 400 || resp.StatusCode == 403) {
		return errTokenRejected
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("NordVPN API returned HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// ---- account ----

type nordCredentials struct {
	NordlynxPrivateKey string `json:"nordlynx_private_key"`
}

func fetchPrivateKey(ctx context.Context, token string) (string, error) {
	var c nordCredentials
	if err := apiGet(ctx, "/v1/users/services/credentials", token, &c); err != nil {
		return "", err
	}
	if _, err := keyToHex(c.NordlynxPrivateKey); err != nil {
		return "", errors.New("NordVPN did not return a NordLynx key for this account")
	}
	return c.NordlynxPrivateKey, nil
}

func cleanToken(t string) (string, error) {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "token:")
	if len(t) < 20 || len(t) > 200 {
		return "", errors.New("that does not look like a NordVPN access token")
	}
	for _, r := range t {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_') {
			return "", errors.New("the token contains characters a NordVPN token never has")
		}
	}
	return t, nil
}

// ---- servers ----

type Country struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Code        string `json:"code"`
	ServerCount int    `json:"serverCount"`
	Cities      []City `json:"cities"`
}

type City struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	ServerCount int    `json:"serverCount"`
}

func fetchCountries(ctx context.Context) ([]Country, error) {
	var raw []struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		Code        string `json:"code"`
		ServerCount int    `json:"serverCount"`
		Cities      []struct {
			ID          int    `json:"id"`
			Name        string `json:"name"`
			ServerCount int    `json:"serverCount"`
		} `json:"cities"`
	}
	if err := apiGet(ctx, "/v1/servers/countries", "", &raw); err != nil {
		return nil, err
	}
	out := make([]Country, 0, len(raw))
	for _, r := range raw {
		c := Country{ID: r.ID, Name: r.Name, Code: r.Code, ServerCount: r.ServerCount}
		for _, ci := range r.Cities {
			c.Cities = append(c.Cities, City{ID: ci.ID, Name: ci.Name, ServerCount: ci.ServerCount})
		}
		sort.Slice(c.Cities, func(i, j int) bool { return c.Cities[i].Name < c.Cities[j].Name })
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type Server struct {
	Hostname    string `json:"hostname"`
	Name        string `json:"name"`
	IP          string `json:"ip"`
	Load        int    `json:"load"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	CountryID   int    `json:"countryId"`
	City        string `json:"city"`
	PublicKey   string `json:"-"`
}

type apiServer struct {
	Name      string `json:"name"`
	Station   string `json:"station"`
	Hostname  string `json:"hostname"`
	Load      int    `json:"load"`
	Status    string `json:"status"`
	Locations []struct {
		Country struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
			Code string `json:"code"`
			City struct {
				Name string `json:"name"`
			} `json:"city"`
		} `json:"country"`
	} `json:"locations"`
	Technologies []struct {
		Identifier string `json:"identifier"`
		Metadata   []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"metadata"`
	} `json:"technologies"`
}

func (a apiServer) toServer() (Server, bool) {
	s := Server{Hostname: a.Hostname, Name: a.Name, IP: a.Station, Load: a.Load}
	if a.Status != "" && a.Status != "online" {
		return s, false
	}
	if ip := net.ParseIP(a.Station); ip == nil || ip.To4() == nil {
		return s, false
	}
	if len(a.Locations) > 0 {
		l := a.Locations[0].Country
		s.Country, s.CountryCode, s.CountryID, s.City = l.Name, l.Code, l.ID, l.City.Name
	}
	for _, t := range a.Technologies {
		if t.Identifier != "wireguard_udp" {
			continue
		}
		for _, m := range t.Metadata {
			if m.Name == "public_key" {
				s.PublicKey = m.Value
			}
		}
	}
	if _, err := keyToHex(s.PublicKey); err != nil {
		return s, false
	}
	return s, true
}

// recommendServers asks NordVPN for the best NordLynx servers for a target.
// The API ranks by distance from the caller's IP and current load.
func recommendServers(ctx context.Context, t Target, limit int) ([]Server, error) {
	q := fmt.Sprintf("/v1/servers/recommendations?filters[servers_technologies][identifier]=wireguard_udp&limit=%d", limit)
	switch t.Kind {
	case "country":
		q += fmt.Sprintf("&filters[country_id]=%d", t.CountryID)
	case "city":
		q += fmt.Sprintf("&filters[country_city_id]=%d", t.CityID)
	}
	var raw []apiServer
	if err := apiGet(ctx, q, "", &raw); err != nil {
		return nil, err
	}
	var out []Server
	for _, a := range raw {
		if s, ok := a.toServer(); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("NordVPN has no NordLynx servers online for that location right now")
	}
	return out, nil
}

// ---- "what is my IP" ----

type IPInsight struct {
	IP        string `json:"ip"`
	Country   string `json:"country"`
	City      string `json:"city"`
	ISP       string `json:"isp"`
	Protected bool   `json:"protected"`
	CheckedAt int64  `json:"checkedAt"`
}

func fetchInsight(ctx context.Context) (*IPInsight, error) {
	var in IPInsight
	if err := apiGet(ctx, "/v1/helpers/ips/insights", "", &in); err != nil {
		return nil, err
	}
	in.CheckedAt = time.Now().Unix()
	return &in, nil
}

// keyToHex converts a base64 WireGuard key to the hex form the UAPI wants.
func keyToHex(b64 string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", err
	}
	if len(b) != 32 {
		return "", errors.New("key is not 32 bytes")
	}
	return hex.EncodeToString(b), nil
}
