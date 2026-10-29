// Diagnostics only. Preserve SDK arguments, return values and thrown errors.
(() => {
  const report = stage => { void window.__xupayActionProgress(stage).catch(() => {}) }
  report('sdk_loaded')
  if (typeof window.Stripe !== 'function') return
  try { window.Stripe = new Proxy(window.Stripe, {
    apply(target, receiver, args) {
      const stripe = Reflect.apply(target, receiver, args)
      for (const [name, stage] of [['retrievePaymentIntent', 'preflight'], ['handleNextAction', 'sdk_action']]) {
        const value = stripe[name]
        if (typeof value !== 'function') continue
        // A future SDK may freeze its methods. Missing progress must never
        // prevent the original method from running or change object identity.
        try {
          stripe[name] = async (...args) => {
            report(stage)
            try {
              const result = await Reflect.apply(value, stripe, args)
              report(stage === 'preflight' ? 'preflight_returned' : 'sdk_returned')
              return result
            } catch (error) { report(stage === 'preflight' ? 'preflight_error' : 'sdk_exception'); throw error }
          }
        } catch {}
      }
      return stripe
    }
  }) } catch {}
})()
