package zimbra

// travar: o Zimbra não roda no Windows; nada a travar.
func travar(string) (func(), error) { return func() {}, nil }
