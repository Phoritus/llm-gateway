// Package egress is the HTTP client for calling a URL a user chose, such as
// a custom endpoint, without letting the user point the gateway at its own
// network: a server-side request forgery guard.
//
// The check runs in the dialer, on the address actually being connected to,
// so a hostname that resolves to a public address when checked and a private
// one when dialed (DNS rebinding) is still refused. Only https is called, no
// proxy is taken from the environment, and redirects are not followed.
package egress
