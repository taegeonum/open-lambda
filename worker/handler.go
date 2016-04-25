package main

import (
	"log"
	"sync"

	state "github.com/tylerharter/open-lambda/worker/handler_state"
)

type HandlerSetOpts struct {
	cm  *ContainerManager
	lru *HandlerLRU
}

type HandlerSet struct {
	mutex    sync.Mutex
	handlers map[string]*Handler
	cm       *ContainerManager
	lru      *HandlerLRU
}

type Handler struct {
	mutex   sync.Mutex
	hset    *HandlerSet
	name    string
	state   state.HandlerState
	runners int
}

func NewHandlerSet(opts HandlerSetOpts) (handlerSet *HandlerSet) {
	if opts.lru == nil {
		opts.lru = NewHandlerLRU(0)
	}

	return &HandlerSet{
		handlers: make(map[string]*Handler),
		cm:       opts.cm,
		lru:      opts.lru,
	}
}

// always return a Handler, creating one if necessarily.
func (h *HandlerSet) Get(name string) *Handler {
	h.mutex.Lock()
	handler := h.handlers[name]
	if handler == nil {
		handler = &Handler{
			hset:    h,
			name:    name,
			state:   state.Unitialized,
			runners: 0,
		}
		h.handlers[name] = handler
	}
	h.mutex.Unlock()

	return handler
}

func (h *Handler) RunStart() (port string, err error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if err := h.maybeInit(); err != nil {
		return "", err
	}

	cm := h.hset.cm

	// are we the first?
	if h.runners == 0 {
		if h.state == state.Stopped {
			if err := cm.DockerRestart(h.name); err != nil {
				return "", err
			}
		} else if h.state == state.Paused {
			if err := cm.DockerUnpause(h.name); err != nil {
				return "", err
			}
		}
		h.state = state.Running
		h.hset.lru.Remove(h)
	}

	h.runners += 1

	port, err = cm.getLambdaPort(h.name)
	if err != nil {
		return "", err
	}

	return port, nil
}

func (h *Handler) RunFinish() {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	cm := h.hset.cm

	h.runners -= 1

	// are we the first?
	if h.runners == 0 {
		if err := cm.DockerPause(h.name); err != nil {
			// TODO(tyler): better way to handle this?  If
			// we can't pause, the handler gets to keep
			// running for free...
			log.Printf("Could not pause %v!  Error: %v\n", h.name, err)
		}
		h.state = state.Paused
		h.hset.lru.Add(h)
	}
}

func (h *Handler) StopIfPaused() {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	cm := h.hset.cm

	if h.state != state.Paused {
		return
	}

	// TODO(tyler): why do we need to unpause in order to kill?
	if err := cm.DockerUnpause(h.name); err != nil {
		log.Printf("Could not unpause %v to kill it!  Error: %v\n", h.name, err)
	} else if err := cm.DockerKill(h.name); err != nil {
		// TODO: a resource leak?
		log.Printf("Could not kill %v after unpausing!  Error: %v\n", h.name, err)
	} else {
		h.state = state.Stopped
	}
}

// assume lock held.  Make sure image is pulled, an determine whether
// container is running.
func (h *Handler) maybeInit() (err error) {
	if h.state != state.Unitialized {
		return nil
	}

	cm := h.hset.cm

	// make sure image is pulled
	img_exists, err := cm.DockerImageExists(h.name)
	if err != nil {
		return err
	}
	if !img_exists {
		if err := cm.DockerPull(h.name); err != nil {
			return err
		}
	}

	// make sure container is created
	cont_exists, err := cm.DockerContainerExists(h.name)
	if err != nil {
		return err
	}
	if !cont_exists {
		if _, err := cm.DockerCreate(h.name, []string{}); err != nil {
			return err
		}
	}

	// is container stopped, running, or started?
	container, err := cm.DockerInspect(h.name)
	if err != nil {
		return err
	}

	if container.State.Running {
		if container.State.Paused {
			h.state = state.Paused
		} else {
			h.state = state.Running
		}
	} else {
		h.state = state.Stopped
	}

	return nil
}