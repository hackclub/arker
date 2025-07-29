package browsermgr

import (
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/playwright-community/playwright-go"
)



type Manager struct {
	mu            sync.RWMutex
	pw            *playwright.Playwright
	browser       playwright.Browser
	launchOpts    playwright.BrowserTypeLaunchOptions
	restarting    bool
	unhealthy     atomic.Bool
	pageSemaphore chan struct{}
	activePagesmu sync.Mutex
	activePages   map[playwright.Page]bool
}

func New(launchOpts playwright.BrowserTypeLaunchOptions, maxWorkers int) (*Manager, error) {
	m := &Manager{
		launchOpts:    launchOpts,
		pageSemaphore: make(chan struct{}, maxWorkers),
		activePages:   make(map[playwright.Page]bool),
	}
	if err := m.start(); err != nil {
		return nil, err
	}
	go m.heartbeat()
	return m, nil
}

func (m *Manager) start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startWithoutLock()
}

func (m *Manager) startWithoutLock() error {
	// Clean up old instances first
	if m.browser != nil {
		m.browser.Close()
	}
	if m.pw != nil {
		m.pw.Stop()
	}
	
	// Clean up semaphore from lost pages during restart
	m.activePagesmu.Lock()
	lostPages := len(m.activePages)
	if lostPages > 0 {
		log.Printf("[browser] cleaning up %d lost pages during restart", lostPages)
		// Release semaphore slots for lost pages (send, not receive!)
		for i := 0; i < lostPages; i++ {
			select {
			case m.pageSemaphore <- struct{}{}:
				// Successfully released a slot
			default:
				log.Printf("[browser] warning: semaphore channel full during restart cleanup")
				break
			}
		}
		// Clear active pages map
		m.activePages = make(map[playwright.Page]bool)
	}
	m.activePagesmu.Unlock()

	pw, err := playwright.Run()
	if err != nil {
		return err
	}
	
	br, err := pw.Chromium.Launch(m.launchOpts)
	if err != nil {
		pw.Stop()
		return err
	}

	// Set up disconnect handler
	br.On("disconnected", func() {
		log.Println("[browser] disconnected event received, scheduling restart")
		go m.restart()
	})

	m.pw, m.browser = pw, br
	m.unhealthy.Store(false)
	
	log.Println("[browser] started successfully")
	return nil
}

func (m *Manager) restart() {
	m.mu.Lock()
	if m.restarting {
		m.mu.Unlock()
		return
	}
	m.restarting = true
	// Keep lock held during restart to prevent concurrent restarts
	defer func() {
		m.restarting = false
		m.mu.Unlock()
	}()

	backoff := []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second}
	for i, d := range backoff {
		if i != 0 {
			log.Printf("[browser] waiting %v before restart attempt %d", d, i+1)
			time.Sleep(d)
		}
		log.Printf("[browser] restarting attempt %d...", i+1)
		if err := m.startWithoutLock(); err == nil {
			log.Println("[browser] restart successful")
			return
		} else {
			log.Printf("[browser] restart attempt %d failed: %v", i+1, err)
		}
	}
	
	log.Println("[browser] unrecoverable - manager marked unhealthy")
	m.unhealthy.Store(true)
}

func (m *Manager) Browser() (playwright.Browser, error) {
	m.mu.RLock()
	br := m.browser
	m.mu.RUnlock()

	if br != nil && br.IsConnected() {
		return br, nil
	}
	
	log.Println("[browser] browser not connected, triggering restart")
	go m.restart()

	// Wait a moment for restart to complete
	time.Sleep(100 * time.Millisecond)
	
	m.mu.RLock()
	br = m.browser
	m.mu.RUnlock()
	
	if br == nil || !br.IsConnected() {
		return nil, errors.New("browser unavailable")
	}
	return br, nil
}

func (m *Manager) NewPage(opts ...playwright.BrowserNewPageOptions) (playwright.Page, error) {
	// Acquire semaphore to limit concurrent pages
	log.Printf("[browser] acquiring page semaphore (capacity: %d)", cap(m.pageSemaphore))
	m.pageSemaphore <- struct{}{}
	log.Printf("[browser] page semaphore acquired (available: %d)", cap(m.pageSemaphore)-len(m.pageSemaphore))
	
	br, err := m.Browser()
	if err != nil {
		// Release semaphore on error
		select {
		case m.pageSemaphore <- struct{}{}:
		default:
		}
		return nil, err
	}
	
	page, err := br.NewPage(opts...)
	if err != nil {
		// Release semaphore on error
		select {
		case m.pageSemaphore <- struct{}{}:
		default:
		}
		return nil, err
	}
	
	// Track active page
	m.activePagesmu.Lock()
	m.activePages[page] = true
	m.activePagesmu.Unlock()
	
	return page, nil
}

// ClosePage manually closes a page and releases semaphore
func (m *Manager) ClosePage(page playwright.Page) error {
	m.activePagesmu.Lock()
	defer m.activePagesmu.Unlock()
	
	if !m.activePages[page] {
		log.Printf("[browser] page not tracked or already closed")
		return nil // Page not tracked or already closed
	}
	
	delete(m.activePages, page)
	err := page.Close()
	
	// Release semaphore slot by sending to channel (not receiving!)
	select {
	case m.pageSemaphore <- struct{}{}:
		log.Printf("[browser] page closed and semaphore released (available: %d)", cap(m.pageSemaphore)-len(m.pageSemaphore))
	default:
		log.Printf("[browser] warning: semaphore channel full during release - possible double release")
	}
	return err
}

func (m *Manager) heartbeat() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for range ticker.C {
		if _, err := m.Browser(); err != nil {
			log.Printf("[browser] heartbeat failed: %v", err)
		}
	}
}

func (m *Manager) Healthy() bool {
	return !m.unhealthy.Load()
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	if m.browser != nil {
		m.browser.Close()
		m.browser = nil
	}
	if m.pw != nil {
		m.pw.Stop()
		m.pw = nil
	}
	log.Println("[browser] closed")
}
