package platform

// These projections intentionally exclude credentials, provider responses, checkout
// links and billing copies. Keep the joins scoped to the authenticated customer.
const customerPriceSummary = `jsonb_strip_nulls(jsonb_build_object(
 'standard',o.price_snapshot->'standard','fee',o.price_snapshot->'fee',
 'tier_code',o.price_snapshot->'tier_code','tier_name',o.price_snapshot->'tier_name',
 'discount',o.price_snapshot->'discount','source',o.price_snapshot->'source'))`

const customerPaymentSummary = `CASE WHEN p.order_id IS NULL THEN '{}'::jsonb ELSE jsonb_build_object(
 'state',p.state,'amount',p.amount,'currency',p.currency,
 'charged_amount',p.charged_amount,'charged_currency',p.charged_currency,
 'last4',p.last4,'fee_state',p.fee_state,
 'message',CASE
 WHEN p.state='action_required' THEN '付款需要额外验证，请联系运营核实。'
 WHEN p.state IN ('review','checking') THEN '付款结果待核实，请勿重复付款。'
 WHEN p.state='failed' AND p.error_code='payment_declined' THEN '付款方式被拒绝，请核对银行卡。'
 WHEN p.state='failed' THEN '本次付款未完成，请查看详情。'
 WHEN p.state='cancelled' THEN '付款准备已结束。'
 ELSE '' END) END`

const customerOrdersQuery = `SELECT o.id,o.product_id,o.product_name,o.email,o.country,o.seats,o.status,o.fee,
 o.source,o.workflow,o.keep_renewal,o.renewal,o.activation,o.created_at,o.updated_at,
 ` + customerPriceSummary + ` AS price_snapshot,` + customerPaymentSummary + ` AS payment
 FROM orders o LEFT JOIN payment_runs p ON p.order_id=o.id AND o.workflow<>'link'
 WHERE o.user_id=$1 AND ($2='' OR o.email ILIKE '%'||$2||'%' OR o.id ILIKE '%'||$2||'%')
 AND ($3='' OR o.status=$3) ORDER BY o.created_at DESC,o.id DESC LIMIT 200`

// Calculate changes before LIMIT, so the oldest visible row still has its actual
// predecessor. For the first ever entry, only use known accounting conventions;
// an old freeze without an order has no reliable amount and remains null.
const customerLedgerQuery = `WITH history AS (
 SELECT id,kind,amount,balance_after,frozen_after,description,created_at,order_id,reference,
 lag(balance_after) OVER (ORDER BY id) AS previous_balance,
 lag(frozen_after) OVER (ORDER BY id) AS previous_frozen
 FROM ledger WHERE user_id=$1
), recent AS (SELECT * FROM history ORDER BY id DESC LIMIT 300)
 SELECT l.id,l.kind,l.amount,l.balance_after,l.frozen_after,l.description,l.created_at,
 CASE WHEN l.previous_balance IS NOT NULL THEN l.balance_after-l.previous_balance
 WHEN l.kind='freeze' THEN -o.fee WHEN l.kind='unfreeze' THEN o.fee
 WHEN l.kind='charge' AND l.reference LIKE 'payment:%' THEN 0
 ELSE l.amount END AS available_delta,
 CASE WHEN l.previous_frozen IS NOT NULL THEN l.frozen_after-l.previous_frozen
 WHEN l.kind='freeze' THEN o.fee WHEN l.kind='unfreeze' THEN -o.fee
 WHEN l.kind='charge' AND l.reference LIKE 'payment:%' THEN l.amount
 ELSE 0 END AS frozen_delta,
 CASE WHEN o.id IS NULL THEN NULL ELSE jsonb_build_object(
 'id',o.id,'product_name',o.product_name,'email',o.email,'country',o.country,'fee',o.fee,
 'price_snapshot',` + customerPriceSummary + `) END AS related_order,
 ` + customerPaymentSummary + ` AS payment
 FROM recent l LEFT JOIN orders o ON o.id=l.order_id AND o.user_id=$1
 LEFT JOIN payment_runs p ON p.order_id=o.id AND o.workflow<>'link'
 ORDER BY l.id DESC`
