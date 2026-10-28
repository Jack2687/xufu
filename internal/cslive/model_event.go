// Adapted from the supplied cs-live-source.zip; see docs/subscription-protocol.md.
package cslive

type model_Event = map[string]any

type model_EventSink func(model_Event)
