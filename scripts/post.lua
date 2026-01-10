-- POST request script for wrk benchmarks
-- Generates a 1KB body for big-request benchmark

local body = string.rep("x", 1024)

wrk.method = "POST"
wrk.body = body
wrk.headers["Content-Type"] = "application/octet-stream"
