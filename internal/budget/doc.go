// Package budget is monthly spending caps in money, per workspace and per
// gateway key: the price table, what an answer costs, and a ledger in Redis
// that counts spend per UTC calendar month and mails the workspace owner at
// 80% and 100% of a budget.
//
// Money is int64 micro-USD everywhere. A price in USD per million tokens is
// micro-USD per token, held as pico-USD per token so that cost is integer
// arithmetic, rounded up to a whole micro-USD per request.
//
// Money is not reserved. A request is checked against what is already spent,
// and its cost is added after it ends, so a budget can be overshot by the
// requests in flight when it fills: at most the workspace's concurrency limit
// of answers. Reserving would need a price for an answer whose length nobody
// knows yet. Spend is counted only when the upstream reported usage, or a
// generation started without reporting any, in which case the request's
// estimate is priced as prompt tokens.
//
// A model with no price row costs nothing and says so (Basis Unpriced), so a
// custom endpoint or a model newer than the table keeps working. A model in
// the free-quota catalog costs nothing (Free), and so does a cache hit
// (Cached).
package budget

// Budgets is everything the data plane asks of budgets.
type Budgets struct {
	*Ledger
	Pricer
}
