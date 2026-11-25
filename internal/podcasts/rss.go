package podcasts

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	reHTMLTags = regexp.MustCompile(`<[^>]*>`)
	reMultiSpaces = regexp.MustCompile(`\s+`)
)

// cleanTextContent elimina marcas HTML, entidades escapadas y espacios redundantes.
func cleanTextContent(raw string) string {
	noHTML := reHTMLTags.ReplaceAllString(raw, " ")
	unescaped := html.UnescapeString(noHTML)
	return strings.TrimSpace(reMultiSpaces.ReplaceAllString(unescaped, " "))
}

// parseDurationSec convierte marcas de tiempo de podcast (HH:MM:SS, MM:SS o segundos) a valor flotante en segundos.
func parseDurationSec(raw string) float64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0
	}

	// Si viene como entero o flotante directo (ej. "2450")
	if sec, err := strconv.ParseFloat(trimmed, 64); err == nil && sec >= 0 {
		return sec
	}

	parts := strings.Split(trimmed, ":")
	if len(parts) == 3 {
		h, _ := strconv.ParseFloat(parts[0], 64)
		m, _ := strconv.ParseFloat(parts[1], 64)
		s, _ := strconv.ParseFloat(parts[2], 64)
		return h*3600 + m*60 + s
	} else if len(parts) == 2 {
		m, _ := strconv.ParseFloat(parts[0], 64)
		s, _ := strconv.ParseFloat(parts[1], 64)
		return m*60 + s
	}

	return 0
}

// xmlRSS representa la raíz del feed RSS 2.0.
type xmlRSS struct {
	XMLName xml.Name   `xml:"rss"`
	Channel xmlChannel `xml:"channel"`
}

// xmlChannel representa el canal o programa principal del podcast.
type xmlChannel struct {
	Title         string    `xml:"title"`
	Description   string    `xml:"description"`
	Link          string    `xml:"link"`
	Author        string    `xml:"author"`
	ItunesAuthor  string    `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd author"`
	ItunesSummary string    `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd summary"`
	ItunesImage   xmlHref   `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
	ChannelImage  xmlImage  `xml:"image"`
	LastBuildDate string    `xml:"lastBuildDate"`
	Items         []xmlItem `xml:"item"`
}

// xmlHref extrae atributos href de etiquetas como itunes:image.
type xmlHref struct {
	Href string `xml:"href,attr"`
}

// xmlImage extrae etiquetas clásicas de imagen RSS <image><url>...</url></image>.
type xmlImage struct {
	URL string `xml:"url"`
}

// xmlItem representa cada episodio individual del podcast.
type xmlItem struct {
	Title          string       `xml:"title"`
	Description    string       `xml:"description"`
	ItunesSummary  string       `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd summary"`
	Link           string       `xml:"link"`
	GUID           xmlGUID      `xml:"guid"`
	PubDate        string       `xml:"pubDate"`
	Enclosure      xmlEnclosure `xml:"enclosure"`
	ItunesDuration string       `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd duration"`
	ItunesImage    xmlHref      `xml:"http://www.itunes.com/dtds/podcast-1.0.dtd image"`
}

// xmlGUID captura el valor de texto del identificador único del episodio.
type xmlGUID struct {
	Value string `xml:",chardata"`
}

// xmlEnclosure captura el archivo de audio adjunto al episodio.
type xmlEnclosure struct {
	URL    string `xml:"url,attr"`
	Length string `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

// ParseFeedXML interpreta el contenido XML de un feed RSS y extrae el podcast con sus episodios de audio.
func ParseFeedXML(data []byte, feedURL string) (*Podcast, []Episode, error) {
	var rss xmlRSS
	if err := xml.Unmarshal(data, &rss); err != nil {
		return nil, nil, fmt.Errorf("error al interpretar XML del feed: %w", err)
	}

	ch := rss.Channel
	title := cleanTextContent(ch.Title)
	if title == "" {
		title = "Podcast sin título"
	}

	author := cleanTextContent(ch.ItunesAuthor)
	if author == "" {
		author = cleanTextContent(ch.Author)
	}
	if author == "" {
		author = "Autor Desconocido"
	}

	desc := cleanTextContent(ch.ItunesSummary)
	if desc == "" {
		desc = cleanTextContent(ch.Description)
	}

	coverURL := strings.TrimSpace(ch.ItunesImage.Href)
	if coverURL == "" {
		coverURL = strings.TrimSpace(ch.ChannelImage.URL)
	}

	p := &Podcast{
		Title:         title,
		Author:        author,
		FeedURL:       strings.TrimSpace(feedURL),
		Description:   desc,
		CoverURL:      coverURL,
		Link:          strings.TrimSpace(ch.Link),
		LastBuildDate: strings.TrimSpace(ch.LastBuildDate),
		CreatedAt:     time.Now(),
	}

	var episodes []Episode
	for _, it := range ch.Items {
		enclosureURL := strings.TrimSpace(it.Enclosure.URL)
		if enclosureURL == "" {
			// Si no tiene archivo multimedia adjunto, se descarta
			continue
		}

		epTitle := cleanTextContent(it.Title)
		if epTitle == "" {
			epTitle = "Episodio sin título"
		}

		epDesc := cleanTextContent(it.ItunesSummary)
		if epDesc == "" {
			epDesc = cleanTextContent(it.Description)
		}

		guid := strings.TrimSpace(it.GUID.Value)
		if guid == "" {
			guid = enclosureURL
		}

		dur := parseDurationSec(it.ItunesDuration)

		encType := strings.TrimSpace(it.Enclosure.Type)
		if encType == "" {
			encType = "audio/mpeg"
		}

		episodes = append(episodes, Episode{
			PodcastTitle:  p.Title,
			PodcastCover:  p.CoverURL,
			GUID:          guid,
			Title:         epTitle,
			Description:   epDesc,
			EnclosureURL:  enclosureURL,
			EnclosureType: encType,
			Duration:      dur,
			PubDate:       strings.TrimSpace(it.PubDate),
			Played:        false,
			Progress:      0,
			CreatedAt:     time.Now(),
		})
	}

	p.EpisodeCount = len(episodes)
	return p, episodes, nil
}

// FetchAndParseFeed descarga un feed RSS desde la red y extrae los datos completos del podcast y sus episodios.
func FetchAndParseFeed(ctx context.Context, client *http.Client, feedURL string) (*Podcast, []Episode, error) {
	cleanURL := strings.TrimSpace(feedURL)
	if cleanURL == "" || strings.ContainsAny(cleanURL, "\r\n\x00") {
		return nil, nil, fmt.Errorf("URL de feed inválida")
	}
	parsedURL, uErr := url.ParseRequestURI(cleanURL)
	if uErr != nil {
		return nil, nil, fmt.Errorf("URL de feed malformada: %w", uErr)
	}
	scheme := strings.ToLower(parsedURL.Scheme)
	if (scheme != "http" && scheme != "https") || parsedURL.Host == "" || parsedURL.User != nil {
		return nil, nil, fmt.Errorf("protocolo o autoridad de feed no permitida")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cleanURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "AuraBox/1.0 PodcastClient (https://github.com/aurabox)")
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml, */*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("error al conectar con el feed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("el servidor de podcast respondió con código HTTP %d", resp.StatusCode)
	}

	// Límite de 8 MB para feeds largos
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("error al leer contenido del feed: %w", err)
	}

	return ParseFeedXML(body, feedURL)
}
