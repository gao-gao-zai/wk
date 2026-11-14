package pool

import (
	"encoding/base64"
	"math"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

// RouteRequest contains digests only. Prefix prevents a reused explicit ID
// with unrelated content from claiming the previous conversation's priority.
type RouteRequest struct {
	Tenant, Session, Prefix string
	Related                 bool
}

type AdmissionConfig struct {
	Enabled, FairQueue   bool
	LeaseMin, LeaseMax   time.Duration
	ReservedSlots        int
	QueueAging           time.Duration
	MaxSessions          int
	MaxPendingPerSession int
}

type routeLease struct {
	tenant, prefix string
	active         int
	success        bool
	lastSuccess    time.Time
	expires        time.Time
	gap            time.Duration
}

func (p *Pool) ConfigureAdmission(c AdmissionConfig) {
	if c.LeaseMin <= 0 {
		c.LeaseMin = 5 * time.Second
	}
	if c.LeaseMax < c.LeaseMin {
		c.LeaseMax = 15 * time.Second
		if c.LeaseMax < c.LeaseMin {
			c.LeaseMax = c.LeaseMin
		}
	}
	if c.ReservedSlots < 0 {
		c.ReservedSlots = 0
	}
	if c.QueueAging <= 0 {
		c.QueueAging = 2 * time.Second
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = 2048
	}
	if c.MaxPendingPerSession <= 0 {
		c.MaxPendingPerSession = 4
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.admission = c
	if p.routeLeases == nil {
		p.routeLeases = make(map[string]map[string]*routeLease)
	}
	if p.serviceTimes == nil {
		p.serviceTimes = make(map[string]time.Duration)
	}
	if p.queueTurns == nil {
		p.queueTurns = make(map[string]*queueTurn)
	}
}

// Route keys also become JSONB object keys in overlap mode. Encode each field
// separately: PostgreSQL JSONB rejects NUL, and '.' is outside the raw URL-safe
// base64 alphabet, preserving all three field boundaries without collisions.
func routeLeaseKey(r RouteRequest) string {
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(r.Tenant)) + "." + encode([]byte(r.Session)) + "." + encode([]byte(r.Prefix))
}

func (p *Pool) RouteActivity(uid string) (active, hot int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.routePressureLocked(uid, time.Now())
}

func hotLease(l *routeLease, now time.Time) bool {
	return l.success && (l.active > 0 || now.Before(l.expires))
}

func (p *Pool) hotRouteLocked(uid string, r RouteRequest, now time.Time) bool {
	if r.Related {
		return true
	}
	l := p.routeLeases[uid][routeLeaseKey(r)]
	return l != nil && l.tenant == r.Tenant && hotLease(l, now)
}

func (p *Pool) routePressureLocked(uid string, now time.Time) (active, hot int) {
	for _, l := range p.routeLeases[uid] {
		if l.active > 0 {
			active++
		}
		if hotLease(l, now) {
			hot++
		}
	}
	return
}

func (p *Pool) pruneLeasesLocked(now time.Time) {
	for uid, leases := range p.routeLeases {
		for key, l := range leases {
			if l.active == 0 && !now.Before(l.expires) {
				delete(leases, key)
				p.leaseCount--
			}
		}
		if len(leases) == 0 {
			delete(p.routeLeases, uid)
		}
	}
}

func (p *Pool) forgetRouteLocked(uid string) {
	p.leaseCount -= len(p.routeLeases[uid])
	delete(p.routeLeases, uid)
	delete(p.serviceTimes, uid)
}

func (p *Pool) startRouteLocked(uid string, r RouteRequest, now time.Time) {
	if !p.admission.Enabled || r.Session == "" || r.Prefix == "" {
		return
	}
	key := routeLeaseKey(r)
	l := p.routeLeases[uid][key]
	if l == nil {
		if p.leaseCount >= p.admission.MaxSessions {
			p.pruneLeasesLocked(now)
			// Never evict a running lease. A new request can still be served
			// when all bounded tracking entries are busy.
			if p.leaseCount >= p.admission.MaxSessions {
				var victimUID, victimKey string
				var oldest time.Time
				for u, leases := range p.routeLeases {
					for k, candidate := range leases {
						if candidate.active == 0 && (victimKey == "" || candidate.lastSuccess.Before(oldest)) {
							victimUID, victimKey, oldest = u, k, candidate.lastSuccess
						}
					}
				}
				if victimKey == "" {
					return
				}
				delete(p.routeLeases[victimUID], victimKey)
				if len(p.routeLeases[victimUID]) == 0 {
					delete(p.routeLeases, victimUID)
				}
				p.leaseCount--
			}
		}
		if p.routeLeases[uid] == nil {
			p.routeLeases[uid] = make(map[string]*routeLease)
		}
		l = &routeLease{tenant: r.Tenant, prefix: r.Prefix, expires: now}
		p.routeLeases[uid][key] = l
		p.leaseCount++
	}
	l.active++
}

// NoteRouteSuccess grants a lease only after a successful upstream request.
// Protection remains valid while its requests run; only success renews grace.
func (p *Pool) NoteRouteSuccess(uid string, r RouteRequest, service time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.admission.Enabled {
		return
	}
	if p.byUID[uid] == nil {
		return
	}
	if service > 0 {
		old := p.serviceTimes[uid]
		if old > 0 {
			service = old/5*4 + service/5
		}
		p.serviceTimes[uid] = service
	}
	l := p.routeLeases[uid][routeLeaseKey(r)]
	if l == nil {
		return
	}
	now := time.Now()
	if l.success && now.After(l.lastSuccess) {
		gap := now.Sub(l.lastSuccess)
		if l.gap > 0 {
			gap = l.gap/5*4 + gap/5
		}
		l.gap = gap
	}
	ttl := l.gap + l.gap/2
	if ttl < p.admission.LeaseMin {
		ttl = p.admission.LeaseMin
	}
	if ttl > p.admission.LeaseMax {
		ttl = p.admission.LeaseMax
	}
	l.success, l.lastSuccess, l.expires = true, now, now.Add(ttl)
}

func (p *Pool) ReleaseRoute(uid string, r RouteRequest) {
	if p.SharedEnabled() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if l := p.routeLeases[uid][routeLeaseKey(r)]; l != nil && l.active > 0 {
		l.active--
	}
	if e := p.byUID[uid]; e != nil && e.inFlight.Load() > 0 {
		e.inFlight.Add(-1)
		p.notifyWaiterLocked(uid)
	}
}

func (p *Pool) reservedLocked(uid string, r RouteRequest, now time.Time) bool {
	if !p.admission.Enabled || p.admission.ReservedSlots == 0 || p.maxInFlight <= 1 || p.hotRouteLocked(uid, r, now) {
		return false
	}
	_, hot := p.routePressureLocked(uid, now)
	if hot == 0 {
		return false
	}
	reserved := min(p.admission.ReservedSlots, p.maxInFlight-1)
	return p.byUID[uid].inFlight.Load() >= int64(p.maxInFlight-reserved)
}

func (p *Pool) readyRouteLocked(uid, model string, tried, allow map[string]bool, now time.Time) bool {
	e := p.byUID[uid]
	return e != nil && !tried[uid] && (allow == nil || allow[uid]) && e.healthyForModel(model, now) && !p.inFlightFull(e)
}

func (p *Pool) alternativeLocked(uid, model string, r RouteRequest, tried, allow map[string]bool, now time.Time) bool {
	for other := range p.byUID {
		if other != uid && p.readyRouteLocked(other, model, tried, allow, now) && !p.reservedLocked(other, r, now) {
			return true
		}
	}
	return false
}

// A soft reservation is enforced only when another authorized ready account
// can serve the request. Decision and slot acquisition share the pool lock.
func (p *Pool) PickAndAcquireByUIDForRoute(uid, model string, r RouteRequest, tried, allow map[string]bool) *auth.Auth {
	if p.SharedEnabled() {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	model = normalizeModel(model)
	if !p.readyRouteLocked(uid, model, tried, allow, now) {
		return nil
	}
	if p.reservedLocked(uid, r, now) && p.alternativeLocked(uid, model, r, tried, allow, now) {
		return nil
	}
	e := p.byUID[uid]
	p.markUsedLocked(e)
	e.inFlight.Add(1)
	p.startRouteLocked(uid, r, now)
	return e.a
}

// RecommendRoute is advisory for a new binding. Existing primary bindings are
// never overwritten by this hint. Actual admission is checked again atomically.
func (p *Pool) RecommendRoute(model string, r RouteRequest, tried, allow map[string]bool) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.recommendRouteLocked(normalizeModel(model), r, tried, allow, time.Now())
}

func (p *Pool) recommendRouteLocked(model string, r RouteRequest, tried, allow map[string]bool, now time.Time) string {
	var uids []string
	for uid := range p.byUID {
		if p.readyRouteLocked(uid, model, tried, allow, now) {
			uids = append(uids, uid)
		}
	}
	sort.Strings(uids)
	best := ""
	score := math.Inf(1)
	for _, uid := range uids {
		if p.reservedLocked(uid, r, now) && p.alternativeLocked(uid, model, r, tried, allow, now) {
			continue
		}
		e := p.byUID[uid]
		active, hot := p.routePressureLocked(uid, now)
		service := p.serviceTimes[uid]
		if service <= 0 {
			service = time.Second
		}
		load := service.Seconds() * float64(e.inFlight.Load()+1) / float64(max(1, p.maxInFlight))
		load += .25*float64(hot)*p.admission.LeaseMin.Seconds() + .1*float64(active)
		if load < score || (load == score && best != "" && e.lastUsed.Before(p.byUID[best].lastUsed)) {
			best, score = uid, load
		}
	}
	return best
}

func (p *Pool) PickAndAcquireRoute(model string, r RouteRequest, tried, allow map[string]bool) *auth.Auth {
	if p.SharedEnabled() {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	model = normalizeModel(model)
	uid := p.recommendRouteLocked(model, r, tried, allow, now)
	if uid != "" {
		e := p.byUID[uid]
		p.markUsedLocked(e)
		e.inFlight.Add(1)
		p.startRouteLocked(uid, r, now)
		return e.a
	}
	// Preserve the existing cooldown fallback if no healthy account is ready.
	var a *auth.Auth
	if allow == nil {
		a = p.pickForModelLocked(model, tried)
	} else {
		a = p.pickForModelAllowLocked(model, tried, allow)
	}
	if a != nil {
		p.byUID[a.UID].inFlight.Add(1)
		p.startRouteLocked(a.UID, r, now)
	}
	return a
}
