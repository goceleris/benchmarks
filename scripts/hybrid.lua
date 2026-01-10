-- Hybrid mode benchmark script for wrk
-- Sends a mix of HTTP/1.1 and H2C upgrade requests to test hybrid mux

local counter = 0
local methods = {"http1", "h2c_upgrade"}

request = function()
    counter = counter + 1
    local mode = methods[(counter % 2) + 1]
    
    if mode == "h2c_upgrade" then
        -- HTTP/1.1 to H2C upgrade request
        local headers = {}
        headers["Connection"] = "Upgrade, HTTP2-Settings"
        headers["Upgrade"] = "h2c"
        headers["HTTP2-Settings"] = "AAMAAABkAAQBAAAAAAIAAAAA"  -- Base64 encoded settings
        return wrk.format("GET", "/", headers)
    else
        -- Regular HTTP/1.1 request
        return wrk.format("GET", "/")
    end
end
