const axios = require("axios");
const querystring = require("querystring");

const jwt = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJtZXJjdXJlIjp7InB1Ymxpc2giOlsiKiJdfX0.obDjwCgqtPuIvwBlTxUEmibbBf0zypKCNzNKP7Op2UM";
const hub = "http://mercure_publishing_hub/.well-known/mercure";
const topic = "https://test.com/"

const options = {
    headers: {
        "Authorization": `Bearer ${jwt}`,
        "Content-Type": "application/x-www-form-urlencoded"
    }
};

function sleep(ms) {
    return new Promise(resolve => setTimeout(resolve, ms));
}

let publish = async() => {
    var i = 0;

    while(true) {
        var data = {"data": i };
        var postData = querystring.stringify({ topic: topic, data: JSON.stringify({ topic: topic, data: data })});
        const response = await axios.post(hub, postData, options);
        console.log(`Published Update ID: ${response.data}. Content: ${JSON.stringify(data)}. Topic: ${topic}`);
        i = i + 1;
        await sleep(2000);
    }
}

publish()