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
	"sync"
	"time"
)

const (
	baseURL    = "https://www.trade-tariff.service.gov.uk/api/v2"
	outputFile = "taric_codes_full.csv"
	maxWorkers = 15 // Riduciamo leggermente i workers
	apiTimeout = 30 * time.Second
	maxRetries = 4                      // Numero massimo di tentativi per richiesta
	rateLimit  = 150 * time.Millisecond // Pausa tra le richieste (circa 6-7 rich/sec)
)

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
	rateLimiter = time.NewTicker(rateLimit) // Il nostro regolatore di flusso
)

func makeAPIRequest(url string) ([]byte, error) {
	var body []byte
	var err error

	for i := 0; i < maxRetries; i++ {
		<-rateLimiter.C // Aspetta il "via libera" dal regolatore

		req, reqErr := http.NewRequest(http.MethodGet, url, nil)
		if reqErr != nil {
			return nil, fmt.Errorf("impossibile creare la richiesta per %s: %w", url, reqErr)
		}
		req.Header.Set("Accept", "application/vnd.uktt.v2")

		res, doErr := httpClient.Do(req)
		if doErr != nil {
			err = fmt.Errorf("errore nella richiesta a %s: %w", url, doErr)
			continue // Riprova in caso di errore di rete
		}

		// Gestione del Rate Limit (429) e altri errori server
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			backoff := time.Duration(1<<i) * time.Second // 1s, 2s, 4s, 8s
			log.Printf("Errore %d per %s. Attendo %v e riprovo...", res.StatusCode, url, backoff)
			time.Sleep(backoff)
			err = fmt.Errorf("risposta non valida dopo %d tentativi: status %d", i+1, res.StatusCode)
			res.Body.Close()
			continue // Riprova
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
		return body, nil // Successo
	}
	return nil, err // Tutti i tentativi sono falliti
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
		// Non usiamo log.Printf qui perché makeAPIRequest ha già gestito i tentativi
		return
	}

	var response APIResponse
	if err := json.Unmarshal(body, &response); err != nil {
		log.Printf("Errore parsing JSON per il codice %s: %v\n", commodityCode, err)
		return
	}

	if response.Data.Attributes.Declarable {
		fmt.Printf("Trovato codice finale: %s\n", response.Data.Attributes.GoodsNomenclatureItemID)
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

func processChapter(chapterID string) []TaricEntry {
	fmt.Printf("== Inizio analisi del Capitolo %s ==\n", chapterID)
	var finalEntries []TaricEntry
	visited := make(map[string]bool)
	findDeclarableCommodities(chapterID, visited, &finalEntries)
	fmt.Printf("== Capitolo %s completato. Trovati %d codici finali. ==\n", chapterID, len(finalEntries))
	return finalEntries
}

func main() {
	defer rateLimiter.Stop() // Pulisce il ticker alla fine

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

	var wg sync.WaitGroup
	resultsChan := make(chan []TaricEntry, len(chapterIDs))

	fmt.Printf("Avvio del processo parallelo con %d workers...\n", maxWorkers)

	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, id := range chapterIDs[i*len(chapterIDs)/maxWorkers : (i+1)*len(chapterIDs)/maxWorkers] {
				resultsChan <- processChapter(id)
			}
		}()
	}

	// Bilanciamo il carico per i capitoli rimanenti
	remaining := len(chapterIDs) % maxWorkers
	if remaining > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, id := range chapterIDs[len(chapterIDs)-remaining:] {
				resultsChan <- processChapter(id)
			}
		}()
	}

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	var allFoundEntries []TaricEntry
	for entries := range resultsChan {
		if entries != nil {
			allFoundEntries = append(allFoundEntries, entries...)
		}
	}

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
