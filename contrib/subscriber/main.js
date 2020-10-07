const EventSource = require("eventsource");

const hub = "http://mercure_subscription_hub/.well-known/mercure";
const topic = "https://test.com/"

var url = new URL(hub)
url.searchParams.append('topic', topic);

var es = new EventSource(url.toString())
es.onmessage = e => console.log(e);
es.onerror = function (err) {
    if (err) {
        if (err.status === 401 || err.status === 403) {
            console.log('not authorized');
        }
    }
};