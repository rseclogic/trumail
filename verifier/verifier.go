package verifier

import (
	"encoding/xml"
	"errors"
	"net/http"

	"golang.org/x/sync/errgroup"
)

// ErrEmailParseFailure is thrown when a non-parsable email is passed
var ErrEmailParseFailure = errors.New("Failed to parse email address")

// Lookup contains all output data for an email validation Lookup
type Lookup struct {
	XMLName      xml.Name `json:"-" xml:"lookup"`
	Address      string   `json:"address,omitempty" xml:"address,omitempty"`
	Username     string   `json:"username,omitempty" xml:"username,omitempty"`
	Domain       string   `json:"domain,omitempty" xml:"domain,omitempty"`
	HostExists   bool     `json:"hostExists" xml:"hostExists"`
	Deliverable  bool     `json:"deliverable" xml:"deliverable"`
	FullInbox    bool     `json:"fullInbox" xml:"fullInbox"`
	CatchAll     bool     `json:"catchAll" xml:"catchAll"`
	Disposable   bool     `json:"disposable" xml:"disposable"`
	Gravatar     bool     `json:"gravatar" xml:"gravatar"`
	Error        string   `json:"error,omitempty" xml:"error,omitempty"`
	ErrorDetails string   `json:"errorDetails,omitempty" xml:"errorDetails,omitempty"`
}

// Verifier contains all data needed to perform educated email verification
// lookups
type Verifier struct {
	client         *http.Client
	maxWorkerCount int    // Maximum number of concurrent domain validation workers
	hostname       string // This machines hostname
	sourceAddr     string // The source email address
	disposabler    *Disposabler
}

// NewVerifier generates a new AddressVerifier reference
func NewVerifier(client *http.Client, maxWorkerCount int, hostname, sourceAddr string) *Verifier {
	return &Verifier{
		client:         client,
		maxWorkerCount: maxWorkerCount,
		hostname:       hostname,
		sourceAddr:     sourceAddr,
		disposabler:    NewDisposabler(client),
	}
}

// VerifySingle parses and verifies a single email address
// returning an error if there's a problem when parsing
func (v *Verifier) VerifySingle(email string) (*Lookup, error) {
	address, err := ParseAddress(email)
	if err != nil {
		return nil, ErrEmailParseFailure
	}
	return v.Verify(address)[0], nil
}

// Verify performs all threaded operations involved with validating
// one or more email addresses
func (v *Verifier) Verify(addresses ...*Address) []*Lookup {
	var totalLookups int
	var lookups []*Lookup

	// Organize all the addresses into a map of domain - address, address...
	domainQueue := make(map[string][]*Address)
	for _, address := range addresses {
		domainQueue[address.Domain] = append(domainQueue[address.Domain], address)
		totalLookups++
	}

	// Don't create channels or workers if there's no work to do
	if len(domainQueue) == 0 {
		return lookups
	}

	// Makes two channels that hold both a queue of Addresses and results
	// of all validations that take place
	jobs := make(chan []*Address, len(domainQueue))
	results := make(chan *Lookup, totalLookups)

	// Generate NO MORE than v.maxWorkerCount workers
	workers := v.maxWorkerCount
	if len(domainQueue) < workers {
		workers = len(domainQueue)
	}

	// For as long as workers specifies, generate a goroutine to Verify every
	// address on the same connection
	for w := 1; w <= workers; w++ {
		go v.worker(jobs, results)
	}

	// Dump a collection of jobs for each domain onto the jobs channel
	for _, addresses := range domainQueue {
		jobs <- addresses
	}
	close(jobs)

	// Pull all the results out of the Lookup results channel and return
	for w := 1; w <= len(addresses); w++ {
		lookups = append(lookups, <-results)
	}
	return lookups
}

// worker receives a domain, an array of addresses and a channel where
// we can place the validation results. Workers are generated for each domain
// and the deliverabler connection is closed once finished
func (v *Verifier) worker(jobs <-chan []*Address, results chan<- *Lookup) {
	for j := range jobs {
		// Defines the domain specific constant variables
		var disposable, catchAll bool
		hostExists := true // Default host existence to true
		var basicErr, detailErr error

		// Attempts to form an SMTP Connection and returns either a Deliverabler
		// or an error which will be parsed and returned in the lookup
		deliverabler, err := NewDeliverabler(j[0].Domain, v.hostname, v.sourceAddr, v.client.Timeout)
		if err != nil {
			basicErr, detailErr = parseSTDErr(err)
			if basicErr == ErrNoSuchHost {
				basicErr = nil
				detailErr = nil
				hostExists = false
			}
		}

		// Retrieves the catchall status if there's a deliverabler and we don't yet
		// have any catchall status
		if deliverabler != nil {
			if deliverabler.HasCatchAll(j[0].Domain, 5) {
				catchAll = true
			}
		}
		disposable = v.disposabler.IsDisposable(j[0].Domain)

		// Builds a validation for every email defined for the domain
		for _, address := range j {
			// Performs address specific validation
			var deliverable, fullInbox, gravatar bool
			var g errgroup.Group

			// Concurrently retrieve final validation info
			g.Go(func() error {
				if catchAll {
					deliverable = true // Catchall domains will always be deliverable
				} else if deliverabler != nil {
					if err := deliverabler.IsDeliverable(address.Address, 3); err != nil {
						if err == ErrFullInbox {
							fullInbox = true
						}
						basicErr, detailErr = parseRCPTErr(err)
						if basicErr == ErrNoSuchHost {
							basicErr = nil
							detailErr = nil
							hostExists = false
						}
					} else {
						deliverable = true
					}
				}
				return nil
			})
			g.Go(func() error {
				gravatar = v.HasGravatar(address)
				return nil
			})
			g.Wait()

			// Add each new validation Lookup to the results channel
			results <- &Lookup{
				Address:      address.Address,
				Username:     address.Username,
				Domain:       address.Domain,
				HostExists:   hostExists,
				Deliverable:  deliverable,
				FullInbox:    fullInbox,
				Disposable:   disposable,
				CatchAll:     catchAll,
				Gravatar:     gravatar,
				Error:        errStr(basicErr),
				ErrorDetails: errStr(detailErr),
			}
		}

		// Close the connection with the MX server now that we've iterated over
		// addresses we're interested in for this server
		if deliverabler != nil {
			deliverabler.Close()
		}
	}
}
