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
	mu          sync.RWMutex
	pw          *playwright.Playwright
	browser     playwright.Browser
	launchOpts  playwright.BrowserTypeLaunchOptions
	restarting  bool
	unhealthy   atomic.Bool
}

func New(launchOpts playwright.BrowserTypeLaunchOptions) (*Manager, error) {
	m := &Manager{launchOpts: launchOpts}
	if err := m.start(); err != nil {
		return nil, err
	}
	go m.heartbeat()
	return m, nil
}

func (m *Manager) start() error {
	// Clean up old instances first
	m.mu.Lock()
	if m.browser != nil {
		m.browser.Close()
	}
	if m.pw != nil {
		m.pw.Stop()
	}
	m.mu.Unlock()

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

	m.mu.Lock()
	m.pw, m.browser = pw, br
	m.unhealthy.Store(false)
	m.mu.Unlock()
	
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
	m.mu.Unlock()

	backoff := []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second}
	for i, d := range backoff {
		if i != 0 {
			log.Printf("[browser] waiting %v before restart attempt %d", d, i+1)
			time.Sleep(d)
		}
		log.Printf("[browser] restarting attempt %d...", i+1)
		if err := m.start(); err == nil {
			m.mu.Lock()
			m.restarting = false
			m.mu.Unlock()
			log.Println("[browser] restart successful")
			return
		} else {
			log.Printf("[browser] restart attempt %d failed: %v", i+1, err)
		}
	}
	
	log.Println("[browser] unrecoverable - manager marked unhealthy")
	m.unhealthy.Store(true)
	m.mu.Lock()
	m.restarting = false
	m.mu.Unlock()
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
	br, err := m.Browser()
	if err != nil {
		return nil, err
	}
	return br.NewPage(opts...)
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
