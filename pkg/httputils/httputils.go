package http

func IsSuccessfulCode(code int) bool {
	return code >= 200 && code < 300
}
