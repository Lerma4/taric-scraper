package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	baseURL    = "https://www.trade-tariff.service.gov.uk/api/v2"
	outputFile = "taric_codes_full.csv"
	maxWorkers = 25
	apiTimeout = 30 * time.Second
	rateLimit  = 150 * time.Millisecond
	maxRetries = 4
)

// Structs per il JSON (invariate)
type Attributes struct {
	GoodsNomenclatureItemID string `json:"goods_nomenclature_item_id"`
	Description             string `json:"description"`
	Declarable              bool   `json:"declarable"`
}

type Item struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Attributes Attributes `json:"attributes"`
}

type APIResponse struct {
	Data     Item   `json:"data"`
	Included []Item `json:"included"`
}

type ChapterListResponse struct {
	Data []Item `json:"data"`
}

type TaricEntry struct {
	Code        string
	Description string
}

var (
	httpClient  = &http.Client{Timeout: apiTimeout}
	rateLimiter = time.NewTicker(rateLimit)
)

func makeAPIRequest(url string) ([]byte, error) {
	var body []byte
	var err error

	for i := 0; i < maxRetries; i++ {
		<-rateLimiter.C

		req, reqErr := http.NewRequest(http.MethodGet, url, nil)
		if reqErr != nil {
			return nil, fmt.Errorf("impossibile creare la richiesta per %s: %w", url, reqErr)
		}
		req.Header.Set("Accept", "application/vnd.uktt.v2")

		res, doErr := httpClient.Do(req)
		if doErr != nil {
			err = fmt.Errorf("errore di rete per %s: %w", url, doErr)
			time.Sleep(time.Duration(1<<i) * time.Second)
			continue
		}

		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			backoff := time.Duration(1<<i) * time.Second
			log.Printf("Errore %d per %s. Attendo %v e riprovo...", res.StatusCode, url, backoff)
			time.Sleep(backoff)
			err = fmt.Errorf("risposta non valida dopo %d tentativi: status %d", i+1, res.StatusCode)
			res.Body.Close()
			continue
		}

		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return nil, fmt.Errorf("risposta non valida da %s: status %d", url, res.StatusCode)
		}

		body, err = io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("errore nella lettura della risposta: %w", err)
		}
		return body, nil
	}
	return nil, err
}

func findDeclarableCommodities(commodityCode string, visited map[string]bool, finalEntries *[]TaricEntry) {
	if visited[commodityCode] {
		return
	}
	visited[commodityCode] = true

	endpointType := "commodities"
	if len(commodityCode) == 2 {
		endpointType = "chapters"
	}

	url := fmt.Sprintf("%s/%s/%s", baseURL, endpointType, commodityCode)
	body, err := makeAPIRequest(url)
	if err != nil {
		return
	}

	var response APIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		log.Printf("Errore parsing JSON per il codice %s: %v\n", commodityCode, err)
		return
	}

	if response.Data.Attributes.Declarable {
		*finalEntries = append(*finalEntries, TaricEntry{
			Code:        response.Data.Attributes.GoodsNomenclatureItemID,
			Description: response.Data.Attributes.Description,
		})
	}

	if len(response.Included) > 0 {
		for _, child := range response.Included {
			if child.Type == "commodity" || child.Type == "heading" {
				findDeclarableCommodities(child.Attributes.GoodsNomenclatureItemID, visited, finalEntries)
			}
		}
	}
}

// La funzione worker ora non stampa più nulla
func processChapter(chapterID string) []TaricEntry {
	var finalEntries []TaricEntry
	visited := make(map[string]bool)
	findDeclarableCommodities(chapterID, visited, &finalEntries)
	return finalEntries
}

// Funzione per disegnare la barra del progresso
func printProgressBar(completed, total int) {
	barWidth := 50
	percent := float64(completed) / float64(total)
	filledWidth := int(float64(barWidth) * percent)

	bar := strings.Repeat("=", filledWidth) + ">" + strings.Repeat(" ", barWidth-filledWidth)
	fmt.Printf("\r[%s] %d/%d (%.0f%%)", bar, completed, total, percent*100)
}

func main() {
	defer rateLimiter.Stop()

	fmt.Println("Recupero la lista dei capitoli...")
	chapterListBody, err := makeAPIRequest(baseURL + "/chapters")
	if err != nil {
		log.Fatalf("Errore critico, impossibile recuperare i capitoli: %v", err)
	}

	var chapterList ChapterListResponse
	if err = json.Unmarshal(chapterListBody, &chapterList); err != nil {
		log.Fatalf("Impossibile fare il parsing della lista capitoli: %v", err)
	}

	var chapterIDs []string
	for _, chap := range chapterList.Data {
		chapterIDs = append(chapterIDs, chap.Attributes.GoodsNomenclatureItemID[:2])
	}

	totalChapters := len(chapterIDs)
	var completedChapters int32 = 0

	jobs := make(chan string, totalChapters)
	resultsChan := make(chan []TaricEntry, totalChapters)

	// Lancia la goroutine per la barra del progresso
	done := make(chan bool)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				// Legge il contatore in modo atomico/sicuro
				completed := atomic.LoadInt32(&completedChapters)
				printProgressBar(int(completed), totalChapters)
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chapterID := range jobs {
				// Il worker processa il capitolo...
				result := processChapter(chapterID)
				// ...invia il risultato...
				resultsChan <- result
				// ...e infine incrementa il contatore.
				atomic.AddInt32(&completedChapters, 1)
			}
		}()
	}

	fmt.Printf("Avvio del processo di analisi con %d workers...\n", maxWorkers)

	for _, id := range chapterIDs {
		jobs <- id
	}
	close(jobs)

	// Goroutine per attendere che i risultati vengano raccolti
	var collectWg sync.WaitGroup
	collectWg.Add(1)
	var allFoundEntries []TaricEntry
	go func() {
		defer collectWg.Done()
		for entries := range resultsChan {
			if entries != nil {
				allFoundEntries = append(allFoundEntries, entries...)
			}
		}
	}()

	wg.Wait()
	close(resultsChan)
	collectWg.Wait()

	// Ferma la goroutine della barra del progresso
	done <- true

	// Assicura che l'ultima versione della barra sia stampata al 100%
	printProgressBar(totalChapters, totalChapters)
	fmt.Println("\nProcesso di download completato. Scrittura del file...")

	seen := make(map[string]bool)
	var uniqueEntries []TaricEntry
	for _, entry := range allFoundEntries {
		if !seen[entry.Code] {
			seen[entry.Code] = true
			uniqueEntries = append(uniqueEntries, entry)
		}
	}

	sort.Slice(uniqueEntries, func(i, j int) bool {
		return uniqueEntries[i].Code < uniqueEntries[j].Code
	})

	file, err := os.Create(outputFile)
	if err != nil {
		log.Fatalf("Impossibile creare il file di output: %v", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if err = writer.Write([]string{"Code", "Description"}); err != nil {
		log.Fatalf("Impossibile scrivere l'intestazione del CSV: %v", err)
	}

	for _, entry := range uniqueEntries {
		if err = writer.Write([]string{entry.Code, entry.Description}); err != nil {
			log.Printf("Attenzione: impossibile scrivere la riga per il codice %s: %v", entry.Code, err)
		}
	}

	fmt.Printf("Operazione completata. Trovati %d codici unici.\n", len(uniqueEntries))
	fmt.Printf("I risultati sono stati salvati nel file: %s\n", outputFile)
}
