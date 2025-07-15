# TARIC Code Scraper

## Overview
This Go application retrieves TARIC (Integrated Tariff of the European Communities) codes from the UK Trade Tariff API. It efficiently scrapes all declarable commodity codes along with their descriptions and saves them to a CSV file.

## Features
- Concurrent processing of multiple chapters using goroutines
- Rate limiting to respect API constraints
- Automatic retries with exponential backoff
- Deduplication of results
- CSV output with code and description

## Requirements
- Go 1.24 or higher

## Usage

Simply run main.go:

```bash
go run main.go
```

The application will:
1. Fetch all available chapters from the UK Trade Tariff API
2. Process each chapter to find all declarable commodity codes
3. Save the results to `taric_codes_full.csv` in the current directory

## Configuration

You can modify the following constants in `main.go` to adjust the behavior:

- `maxWorkers`: Number of concurrent workers (default: 15)
- `apiTimeout`: HTTP client timeout (default: 30 seconds)
- `maxRetries`: Maximum number of retry attempts (default: 4)
- `rateLimit`: Time between API requests (default: 150ms)
- `outputFile`: Name of the output CSV file

## Output Format

The output CSV file contains two columns:
- `Code`: The 10-digit TARIC code
- `Description`: The description of the commodity

## License

This project is open source and available under the [MIT License](LICENSE).