FROM node:latest

# Create app directory
RUN mkdir -p /usr/src/app
WORKDIR /usr/src/app

# Install dependencies
COPY package.json .
RUN npm install --dev

# Bundle app source
COPY . .

RUN npm install -g nodemon

# Exports
EXPOSE 3000
CMD [ "npm", "run", "start.dev" ]