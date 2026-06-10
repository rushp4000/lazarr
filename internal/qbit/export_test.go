package qbit

// PollWaitDownloadsForTest runs one wait-poller tick synchronously (the 20s ticker
// is impractical in unit tests). Compiled only for tests.
func PollWaitDownloadsForTest(s Server) {
	if srv, ok := s.(*server); ok {
		srv.pollWaitDownloads()
	}
}
