-- Detailed latency measurement script for wrk2
-- Captures high-precision latency percentiles

done = function(summary, latency, requests)
    io.write("------------------------------\n")
    io.write("Latency Distribution (HdrHistogram)\n")
    io.write("------------------------------\n")
    
    for _, p in pairs({ 50, 75, 90, 99, 99.9, 99.99, 99.999, 100 }) do
        n = latency:percentile(p)
        io.write(string.format("%7.3f%% %s\n", p, format_time(n)))
    end
    
    io.write("------------------------------\n")
    io.write(string.format("Requests:    %d\n", summary.requests))
    io.write(string.format("Duration:    %s\n", format_time(summary.duration)))
    io.write(string.format("Errors:      %d\n", summary.errors.connect + summary.errors.read + summary.errors.write + summary.errors.status + summary.errors.timeout))
end

function format_time(us)
    if us < 1000 then
        return string.format("%.2fus", us)
    elseif us < 1000000 then
        return string.format("%.2fms", us/1000)
    else
        return string.format("%.2fs", us/1000000)
    end
end
