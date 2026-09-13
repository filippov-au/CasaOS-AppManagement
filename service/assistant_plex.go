package service

import (
	"context"
	stdjson "encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/IceWhaleTech/CasaOS-AppManagement/internal/assistant"
)

func assistantPlexClaimed(data []byte) (bool, error) {
	var value interface{}
	if strings.HasPrefix(strings.TrimSpace(string(data)), "<") {
		var identity struct {
			Claimed string `xml:"claimed,attr"`
		}
		if err := xml.Unmarshal(data, &identity); err != nil {
			return false, err
		}
		value = identity.Claimed
	} else {
		var identity struct {
			Container map[string]interface{} `json:"MediaContainer"`
		}
		if err := stdjson.Unmarshal(data, &identity); err != nil {
			return false, err
		}
		value = identity.Container["claimed"]
	}
	switch v := value.(type) {
	case bool:
		return v, nil
	case float64:
		if v == 0 || v == 1 {
			return v == 1, nil
		}
	case string:
		if v == "0" || v == "1" || v == "true" || v == "false" {
			return v == "1" || v == "true", nil
		}
	}
	return false, errors.New("Plex did not report whether the server is claimed")
}

func assistantPlexSetup(ctx context.Context, m *assistantMediaTarget) (string, error) {
	data, err := m.request(ctx, http.MethodGet, "/identity", nil)
	if err != nil {
		return "", err
	}
	claimed, err := assistantPlexClaimed(data)
	if err != nil {
		return "", err
	}
	base, err := url.Parse(m.base)
	if err != nil {
		return "", err
	}
	port, _ := strconv.Atoi(base.Port())
	result := map[string]interface{}{"app": m.app.Name, "claimed": claimed, "published_port": port, "web_path": "/web"}
	if claimed {
		_, err = m.request(ctx, http.MethodGet, "/library/sections", nil)
		result["libraries_accessible"] = err == nil
		if err != nil {
			result["next_step"] = "The server is claimed, but its libraries are not accessible with the saved token. Ask the owner to sign in through Plex Web and check server access; do not replace the account."
		} else {
			result["next_step"] = "Account claiming is complete. Configure media libraries and verify their paths."
		}
	} else {
		result["claim_url"] = "https://plex.tv/claim"
		result["credential_name"] = "plex_claim"
		result["next_step"] = "Ask the user to sign in at the claim URL and add the new token through Add private credential as plex_claim. Tokens expire within 4 minutes. For LinuxServer Plex use configure_service with environment PLEX_CLAIM=$secret:plex_claim; then read setup again until claimed is true. Open the published port at /web to finish account setup if needed. Do not claim success from container startup alone."
	}
	return assistantJSON(result), nil
}

type assistantPlexSection struct {
	Key       string `json:"key" xml:"key,attr"`
	Title     string `json:"title" xml:"title,attr"`
	Type      string `json:"type" xml:"type,attr"`
	Locations []struct {
		Path string `json:"path" xml:"path,attr"`
	} `json:"Location" xml:"Location"`
}

func assistantPlexSections(data []byte) ([]assistantPlexSection, error) {
	if strings.HasPrefix(strings.TrimSpace(string(data)), "<") {
		var doc struct {
			Directories []assistantPlexSection `xml:"Directory"`
		}
		err := xml.Unmarshal(data, &doc)
		return doc.Directories, err
	}
	var doc struct {
		MediaContainer struct {
			Directory []assistantPlexSection `json:"Directory"`
		} `json:"MediaContainer"`
	}
	err := stdjson.Unmarshal(data, &doc)
	return doc.MediaContainer.Directory, err
}
func assistantPlexLibraryPlan(ctx context.Context, raw stdjson.RawMessage) (assistant.Prepared, error) {
	var a struct {
		App     string `json:"app"`
		Service string `json:"service"`
		Name    string `json:"name"`
		Path    string `json:"path"`
		Type    string `json:"type"`
	}
	if err := assistantDecode(raw, &a); err != nil {
		return assistant.Prepared{}, err
	}
	if strings.TrimSpace(a.Name) == "" || len(a.Name) > 100 || (a.Type != "show" && a.Type != "movie") {
		return assistant.Prepared{}, errors.New("provide a library name and type show or movie")
	}
	m, err := assistantMediaResolve(ctx, a.App, a.Service)
	if err != nil {
		return assistant.Prepared{}, err
	}
	if m.kind != "plex" {
		return assistant.Prepared{}, errors.New("select a Plex service")
	}
	if _, err = assistantMediaPath(m.app, a.Service, a.Path, false); err != nil {
		return assistant.Prepared{}, err
	}
	return assistant.Prepared{Summary: "Create Plex " + a.Type + " library “" + a.Name + "” at " + a.Path + " for " + a.App + " / " + a.Service + ". Plex may scan files and retrieve metadata. Existing libraries and media are preserved.", Execute: func(ctx context.Context) (string, error) {
		unlock, err := LockAppOperation(a.App)
		if err != nil {
			return "", err
		}
		defer unlock()
		fresh, err := assistantSameMedia(ctx, m, a.Service)
		if err != nil {
			return "", err
		}
		if _, err = assistantMediaPath(fresh.app, a.Service, a.Path, false); err != nil {
			return "", err
		}
		check := func() (bool, error) {
			data, e := m.request(ctx, http.MethodGet, "/library/sections", nil)
			if e != nil {
				return false, e
			}
			sections, e := assistantPlexSections(data)
			if e != nil {
				return false, e
			}
			for _, s := range sections {
				if s.Title == a.Name {
					if s.Type != a.Type {
						return false, errors.New("a library with this name has another type")
					}
					for _, loc := range s.Locations {
						if loc.Path == a.Path {
							return true, nil
						}
					}
					return false, errors.New("a library with this name has another path; choose a new name")
				}
			}
			return false, nil
		}
		found, err := check()
		if err != nil {
			return "", err
		}
		if found {
			return "Plex library already configured: " + a.Name, nil
		}
		scanner, agent := "Plex TV Series", "tv.plex.agents.series"
		if a.Type == "movie" {
			scanner = "Plex Movie"
			agent = "tv.plex.agents.movie"
		}
		query := url.Values{"name": {a.Name}, "type": {a.Type}, "agent": {agent}, "scanner": {scanner}, "language": {"en-US"}, "location": {a.Path}}
		readyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		for {
			_, err = m.request(readyCtx, http.MethodPost, "/library/sections?"+query.Encode(), nil)
			if err == nil {
				break
			}
			var httpErr *assistantMediaHTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest || !strings.Contains(httpErr.Message, "still starting up") {
				return "", err
			}
			select {
			case <-readyCtx.Done():
				return "", errors.New("Plex is still initializing; wait and retry library setup")
			case <-time.After(time.Second):
			}
		}
		found, err = check()
		if err != nil {
			return "Library submitted; verify its status in Plex.", err
		}
		if !found {
			return "", errors.New("Plex did not return the new library")
		}
		return "Plex library created and verified: " + a.Name + " (" + a.Path + "). Initial scanning may still be running.", nil
	}}, nil
}
