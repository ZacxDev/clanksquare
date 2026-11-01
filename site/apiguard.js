// Response filters for GET /api/channels (njs). njs runs each request in its
// own VM, so the byte counter below is per request.

// The most bytes of a hub answer passed to the browser. A discovery page is a
// few kilobytes; past this the answer is cut short, so it no longer parses as
// JSON and the page shows "couldn't load channels".
var CAP = 262144;
var sent = 0;
var done = false;

// Whatever the upstream says, the browser gets JSON: a hub (or anything in
// front of it) answering text/html must not be rendered on this origin.
function header(r) {
    r.headersOut['Content-Type'] = 'application/json';
    delete r.headersOut['Content-Length'];
}

function body(r, data, flags) {
    if (done) {
        return;
    }
    if (sent + data.length > CAP) {
        done = true;
        r.sendBuffer(data.slice(0, CAP - sent), { last: true });
        return;
    }
    sent += data.length;
    if (flags.last) {
        done = true;
    }
    r.sendBuffer(data, flags);
}

export default { header, body };
